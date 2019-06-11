// Copyright 2019 DxChain, All rights reserved.
// Use of this source code is governed by an Apache
// License 2.0 that can be found in the LICENSE file.

package storageclient

import (
	"container/heap"
	"errors"
	"github.com/DxChainNetwork/godx/storage"
	"github.com/DxChainNetwork/godx/storage/storageclient/filesystem/dxfile"
	"io/ioutil"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// repairTarget is a helper type for telling the repair heap what type of
// files/Segments to target for repair
type repairTarget int

// targetStuckSegments tells the repair loop to target stuck Segments for repair and
// targetUnstuckSegments tells the repair loop to target unstuck Segments for repair
const (
	targetError repairTarget = iota
	targetStuckSegments
	targetUnstuckSegments
)

// uploadSegmentHeap is a bunch of priority-sorted Segments that need to be either
// uploaded or repaired.
type uploadSegmentHeap []*unfinishedUploadSegment

// Implementation of heap.Interface for uploadSegmentHeap.
func (uch uploadSegmentHeap) Len() int { return len(uch) }
func (uch uploadSegmentHeap) Less(i, j int) bool {
	// If the Segments have the same stuck status, check which Segment has the lower
	// completion percentage.
	if uch[i].stuck == uch[j].stuck {
		return float64(uch[i].sectorsCompletedNum)/float64(uch[i].sectorsNeedNum) < float64(uch[j].sectorsCompletedNum)/float64(uch[j].sectorsNeedNum)
	}
	// If Segment i is stuck, return true to prioritize it.
	if uch[i].stuck {
		return true
	}
	// Segment j is stuck, return false to prioritize it.
	return false
}
func (uch uploadSegmentHeap) Swap(i, j int)       { uch[i], uch[j] = uch[j], uch[i] }
func (uch *uploadSegmentHeap) Push(x interface{}) { *uch = append(*uch, x.(*unfinishedUploadSegment)) }
func (uch *uploadSegmentHeap) Pop() interface{} {
	old := *uch
	n := len(old)
	x := old[n-1]
	*uch = old[0 : n-1]
	return x
}

// uploadHeap contains a priority-sorted heap of all the Segments being uploaded
// to the storage client, along with some metadata.
type uploadHeap struct {
	heap uploadSegmentHeap

	// heapSegments is a map containing all the Segments that are currently in the
	// heap. Segments are added and removed from the map when Segments are pushed
	// and popped off the heap
	//
	// repairingSegments is a map containing all the Segments are that currently
	// assigned to workers and are being repaired/worked on.
	heapSegments      map[uploadSegmentID]struct{}
	repairingSegments map[uploadSegmentID]struct{}

	// Control channels
	newUploads          chan struct{}
	repairNeeded        chan struct{}
	stuckSegmentFound   chan struct{}
	stuckSegmentSuccess chan storage.DxPath

	mu sync.Mutex
}

func (uh *uploadHeap) len() int {
	uh.mu.Lock()
	uhLen := uh.heap.Len()
	uh.mu.Unlock()
	return uhLen
}

func (uh *uploadHeap) push(uuc *unfinishedUploadSegment) bool {
	var added bool
	uh.mu.Lock()
	_, exists1 := uh.heapSegments[uuc.id]
	_, exists2 := uh.repairingSegments[uuc.id]
	if !exists1 && !exists2 {
		uh.heapSegments[uuc.id] = struct{}{}
		heap.Push(&uh.heap, uuc)
		added = true
	}
	uh.mu.Unlock()
	return added
}

func (uh *uploadHeap) pop() (uc *unfinishedUploadSegment) {
	uh.mu.Lock()
	if len(uh.heap) > 0 {
		uc = heap.Pop(&uh.heap).(*unfinishedUploadSegment)
		delete(uh.heapSegments, uc.id)
	}
	uh.mu.Unlock()
	return uc
}

func (sc *StorageClient) createUnfinishedSegments(entry *dxfile.FileSetEntryWithID, hosts map[string]struct{}, target repairTarget, hostHealthInfoTable storage.HostHealthInfoTable) []*unfinishedUploadSegment {
	if len(sc.workerPool) < int(entry.ErasureCode().MinSectors()) {
		sc.log.Debug("cannot create any segment from file because there are not enough workers, so marked all unhealthy segments as stuck")
		if err := entry.MarkAllUnhealthySegmentsAsStuck(hostHealthInfoTable); err != nil {
			sc.log.Debug("WARN: unable to mark all segments as stuck:", err)
		}
		return nil
	}

	// Assemble Segment indexes, stuck Loop should only be adding stuck Segments and
	// the repair loop should only be adding unstuck Segments
	var segmentIndexes []int
	for i := 0; i < entry.NumSegments(); i++ {
		if (target == targetStuckSegments) == entry.GetStuckByIndex(i) {
			segmentIndexes = append(segmentIndexes, i)
		}
	}

	// Sanity check that we have segment indices to go through
	if len(segmentIndexes) == 0 {
		sc.log.Debug("WARN: no segment indices gathered, can't add segments to heap")
		return nil
	}

	// Assemble the set of Segments.
	//
	// TODO / NOTE: Future files may have a different method for determining the
	// number of Segments. Changes will be made due to things like sparse files,
	// and the fact that Segments are going to be different sizes.
	newUnfinishedSegments := make([]*unfinishedUploadSegment, len(segmentIndexes))
	for i, index := range segmentIndexes {
		// Sanity check: fileUID should not be the empty value.
		fid := entry.UID()
		if string(fid[:]) == "" {
			return nil
		}

		// Create unfinishedUploadSegment
		newUnfinishedSegments[i] = &unfinishedUploadSegment{
			fileEntry: entry.CopyEntry(),

			id: uploadSegmentID{
				fid:   fid,
				index: uint64(index),
			},

			index:  uint64(index),
			length: entry.SegmentSize(),
			offset: int64(uint64(index) * entry.SegmentSize()),

			memoryNeeded:   entry.SectorSize()*uint64(entry.ErasureCode().NumSectors()+entry.ErasureCode().MinSectors()) + uint64(entry.ErasureCode().NumSectors())*uint64(entry.CipherKey().Overhead()),
			minimumSectors: int(entry.ErasureCode().MinSectors()),
			sectorsNeedNum: int(entry.ErasureCode().NumSectors()),
			stuck:          entry.GetStuckByIndex(index),

			physicalSegmentData: make([][]byte, entry.ErasureCode().NumSectors()),

			sectorSlotsStatus: make([]bool, entry.ErasureCode().NumSectors()),
			unusedHosts:       make(map[string]struct{}),
		}
		// Every Segment can have a different set of unused hosts.
		for host := range hosts {
			newUnfinishedSegments[i].unusedHosts[host] = struct{}{}
		}
	}

	// Iterate through the pieces of all Segments of the file and mark which
	// hosts are already in use for a particular Segment. As you delete hosts
	// from the 'unusedHosts' map, also increment the 'sectorsCompletedNum' value.
	for i, index := range segmentIndexes {
		sectors, err := entry.Sectors(index)
		if err != nil {
			sc.log.Debug("failed to get sectors for building incomplete Segments")
			return nil
		}
		for sectorIndex, sectorSet := range sectors {
			for _, sector := range sectorSet {
				// Get the contract for the sector
				// TODO get contractUtility from hostmanager
				// contractUtility, exists := sc.storageHostManager.ContractUtility(sector.HostID)
				//if !exists {
				//	// File contract does not seem to be part of the host anymore.
				//	continue
				//}

				contractUtility := storage.ContractUtility{}
				if !contractUtility.GoodForRenew {
					continue
				}

				// Mark the Segment set based on the pieces in this contract
				_, exists := newUnfinishedSegments[i].unusedHosts[sector.HostID.String()]
				redundantSector := newUnfinishedSegments[i].sectorSlotsStatus[sectorIndex]
				if exists && !redundantSector {
					newUnfinishedSegments[i].sectorSlotsStatus[sectorIndex] = true
					newUnfinishedSegments[i].sectorsCompletedNum++
					delete(newUnfinishedSegments[i].unusedHosts, sector.HostID.String())
				} else if exists {
					delete(newUnfinishedSegments[i].unusedHosts, sector.HostID.String())
				}
			}
		}
	}

	// Iterate through the set of newUnfinishedSegments and remove any that are
	// completed or are not downloadable.
	incompleteSegments := newUnfinishedSegments[:0]
	for _, segment := range newUnfinishedSegments {
		// Check if segment is complete
		incomplete := segment.sectorsCompletedNum < segment.sectorsNeedNum

		// Check if segment is downloadable
		segmentHealth := segment.fileEntry.SegmentHealth(int(segment.index), hostHealthInfoTable)
		_, err := os.Stat(string(segment.fileEntry.LocalPath()))
		downloadable := segmentHealth >= dxfile.UnstuckHealthThreshold || err == nil

		// Check if segment seems stuck
		stuck := !incomplete && segmentHealth != dxfile.CompleteHealthThreshold

		// Add Segment to list of incompleteSegments if it is incomplete and
		// downloadable or if we are targeting stuck segments
		if incomplete && (downloadable || target == targetStuckSegments) {
			incompleteSegments = append(incompleteSegments, segment)
			continue
		}

		// If a segment is not downloadable mark it as stuck
		// 当文件上传未达到recoverable级别时，此时又把该源文件删除，则会被永远标记为stuck = true
		if !downloadable {
			sc.log.Info("Marking segment", segment.id, "as stuck due to not being downloadable")
			err = segment.fileEntry.SetStuckByIndex(int(segment.index), true)
			if err != nil {
				sc.log.Debug("WARN: unable to mark segment as stuck:", err)
			}
			continue
		} else if stuck {
			sc.log.Info("Marking segment", segment.id, "as stuck due to being complete but having a health of", segmentHealth)
			err = segment.fileEntry.SetStuckByIndex(int(segment.index), true)
			if err != nil {
				sc.log.Debug("WARN: unable to mark segment as stuck:", err)
			}
			continue
		}

		// Close entry of completed Segment
		err = sc.setStuckAndClose(segment, false)
		if err != nil {
			sc.log.Debug("WARN: unable to mark Segment as stuck and close:", err)
		}
	}
	return incompleteSegments
}

// Select a dxfile randomly and then grab one segment randomly in this file
func (sc *StorageClient) createAndPushRandomSegment(files []*dxfile.FileSetEntryWithID, hosts map[string]struct{}, target repairTarget, hostHealthInfoTable storage.HostHealthInfoTable) {
	// Sanity check that there are files
	if len(files) == 0 {
		return
	}

	// Grab a random file
	randFileIndex := rand.Intn(len(files))
	file := files[randFileIndex]
	sc.lock.Lock()

	// Build the unfinished stuck segments from the file
	unfinishedUploadSegments := sc.createUnfinishedSegments(file, hosts, target, hostHealthInfoTable)
	sc.lock.Unlock()

	// Sanity check that there are stuck segments
	if len(unfinishedUploadSegments) == 0 {
		sc.log.Debug("WARN: no stuck unfinishedUploadSegments returned from buildUnfinishedSegments, so no stuck Segments will be added to the heap")
		return
	}

	// Add a random stuck segment to the upload heap and set its stuckRepair field to true
	randSegmentIndex := rand.Intn(len(unfinishedUploadSegments))
	randSegment := unfinishedUploadSegments[randSegmentIndex]
	randSegment.stuckRepair = true
	if !sc.uploadHeap.push(randSegment) {
		// Segment wasn't added to the heap. Close the file
		err := randSegment.fileEntry.Close()
		if err != nil {
			sc.log.Debug("WARN: unable to close file:", err)
		}
	}
	// Close the unused unfinishedUploadSegments
	unfinishedUploadSegments = append(unfinishedUploadSegments[:randSegmentIndex], unfinishedUploadSegments[randSegmentIndex+1:]...)
	for _, segment := range unfinishedUploadSegments {
		err := segment.fileEntry.Close()
		if err != nil {
			sc.log.Debug("WARN: unable to close file:", err)
		}
	}
	return
}

// createAndPushSegments creates the unfinished segments and push them to the upload heap
func (sc *StorageClient) createAndPushSegments(files []*dxfile.FileSetEntryWithID, hosts map[string]struct{}, target repairTarget, hostHealthInfoTable storage.HostHealthInfoTable) {
	for _, file := range files {
		sc.lock.Lock()
		unfinishedUploadSegments := sc.createUnfinishedSegments(file, hosts, target, hostHealthInfoTable)
		sc.lock.Unlock()
		if len(unfinishedUploadSegments) == 0 {
			sc.log.Debug("No unfinishedUploadSegments returned from buildUnfinishedSegments, so no Segments will be added to the heap")
			continue
		}
		for i := 0; i < len(unfinishedUploadSegments); i++ {
			if !sc.uploadHeap.push(unfinishedUploadSegments[i]) {
				err := unfinishedUploadSegments[i].fileEntry.Close()
				if err != nil {
					sc.log.Debug("WARN: unable to close file:", err)
				}
			}
		}
	}
}

// pushDirToSegmentHeap is charge of creating segment heap that worker tasks locate in
func (sc *StorageClient) pushDirToSegmentHeap(dirDxPath storage.DxPath, hosts map[string]struct{}, target repairTarget) {
	// Get Directory files
	var files []*dxfile.FileSetEntryWithID
	var err error
	fileInfos, err := ioutil.ReadDir(string(dirDxPath.SysPath(sc.fileSystem.FileRootDir())))
	if err != nil {
		return
	}
	for _, fi := range fileInfos {
		// skip sub directories and non dxFiles
		ext := filepath.Ext(fi.Name())
		if fi.IsDir() || ext != storage.DxFileExt {
			continue
		}

		// Open DxFile
		dxPath, err := dirDxPath.Join(strings.TrimSuffix(fi.Name(), ext))
		if err != nil {
			return
		}
		file, err := sc.fileSystem.OpenFile(dxPath)
		if err != nil {
			sc.log.Debug("WARN: could not open dx file:", err)
			continue
		}

		// For stuck Segment repairs, check to see if file has stuck segments
		if target == targetStuckSegments && file.NumStuckSegments() == 0 {
			// Close unneeded files
			err := file.Close()
			if err != nil {
				sc.log.Debug("WARN: Could not close file:", err)
			}
			continue
		}

		// For normal repairs, ignore files that have been recently repaired
		if target == targetUnstuckSegments && time.Since(file.TimeRecentRepair()) < FileRepairInterval {
			// Close unneeded files
			err := file.Close()
			if err != nil {
				sc.log.Debug("WARN: Could not close file:", err)
			}
			continue
		}
		// For normal repairs, ignore files that don't have any unstuck segments
		if target == targetUnstuckSegments && file.NumSegments() == file.NumStuckSegments() {
			err := file.Close()
			if err != nil {
				sc.log.Debug("WARN: Could not close file:", err)
			}
			continue
		}

		files = append(files, file)
	}

	// Check if any files were selected from directory
	if len(files) == 0 {
		sc.log.Debug("No files pulled from `", dirDxPath, "` to build the repair heap")
		return
	}

	// TODO Build the unfinished upload Segments and add them to the upload heap
	// offline, goodForRenew, _ := sc.managedContractUtilityMaps()
	nilHostHealthInfoTable := make(storage.HostHealthInfoTable)

	switch target {
	case targetStuckSegments:
		sc.log.Debug("Adding stuck Segment to heap")
		sc.createAndPushRandomSegment(files, hosts, target, nilHostHealthInfoTable)
	case targetUnstuckSegments:
		sc.log.Debug("Adding Segments to heap")
		sc.createAndPushSegments(files, hosts, target, nilHostHealthInfoTable)
	default:
		sc.log.Debug("WARN: repair target not recognized", target)
	}

	// Close all files
	for _, file := range files {
		err := file.Close()
		if err != nil {
			sc.log.Debug("WARN: Could not close file:", err)
		}
	}
}

// doPrepareNextSegment takes the next segment from the segment heap and prepares it for upload
func (sc *StorageClient) doPrepareNextSegment(uuc *unfinishedUploadSegment) error {
	// Block until there is enough memory, and then upload segment asynchronously
	if !sc.memoryManager.Request(uuc.memoryNeeded, false) {
		return errors.New("can't obtain enough memory")
	}

	// Don't block the outer loop
	go sc.uploadSegment(uuc)
	return nil
}

// refreshHostsAndWorkers will reset the set of hosts and the set of
// workers for the storage client
func (sc *StorageClient) refreshHostsAndWorkers() map[string]struct{} {
	currentContracts := sc.storageHostManager.GetStorageContractSet().Contracts()
	hosts := make(map[string]struct{})
	for _, contract := range currentContracts {
		hosts[contract.Header().EnodeID.String()] = struct{}{}
	}

	// Refresh the worker pool
	sc.activateWorkerPool()
	return hosts
}

// repairLoop works through the upload heap repairing segments. The repair
// loop will continue until the storage client stops, there are no more Segments, or
// enough time has passed indicated by the rebuildHeapSignal
func (sc *StorageClient) uploadLoop(hosts map[string]struct{}) {
	var consecutiveSegmentUploads int
	rebuildHeapSignal := time.After(RebuildSegmentHeapInterval)
	for {
		select {
		case <-sc.tm.StopChan():
			return
		case <-rebuildHeapSignal:
			return
		default:
		}

		// Return if not online.
		if !sc.Online() {
			return
		}

		// Pop the next segment and check whether is empty
		nextSegment := sc.uploadHeap.pop()
		if nextSegment == nil {
			return
		}

		sc.log.Debug("Sending next segment to the workers", nextSegment.id)
		// If the num of workers in worker pool is not enough to cover the tasks, we will
		// mark the segment as stuck
		sc.lock.Lock()
		availableWorkers := len(sc.workerPool)
		sc.lock.Unlock()
		if availableWorkers < nextSegment.minimumSectors {
			sc.log.Debug("Setting segment as stuck because there are not enough good workers", nextSegment.id)
			err := sc.setStuckAndClose(nextSegment, true)
			if err != nil {
				sc.log.Debug("Unable to mark segment as stuck and close:", err)
			}
			continue
		}

		// doPrepareNextSegment block until enough memory of segment and then distribute it to the workers
		err := sc.doPrepareNextSegment(nextSegment)
		if err != nil {
			sc.log.Debug("Unable to prepare next segment without issues", err, nextSegment.id)
			err = sc.setStuckAndClose(nextSegment, true)
			if err != nil {
				sc.log.Debug("Unable to mark segment as stuck and close:", err)
			}
			continue
		}
		consecutiveSegmentUploads++

		// Check if enough segments are currently being repaired
		if consecutiveSegmentUploads >= MaxConsecutiveSegmentUploads {
			var stuckSegments []*unfinishedUploadSegment
			for sc.uploadHeap.len() > 0 {
				if c := sc.uploadHeap.pop(); c.stuck {
					stuckSegments = append(stuckSegments, c)
				}
			}
			for _, ss := range stuckSegments {
				if !sc.uploadHeap.push(ss) {
					err := ss.fileEntry.Close()
					if err != nil {
						sc.log.Debug("Unable to close file:", err)
					}
				}
			}
			return
		}
	}
}

// doUploadAndRepair will find new uploads and existing files in need of
// repair and execute the uploads and repairs. This function effectively runs a
// single iteration of threadedUploadAndRepair.
func (sc *StorageClient) doUploadAndRepair() error {
	// Find the lowest health directory to queue for repairs.
	dxFile, err := sc.fileSystem.SelectDxFileToFix()
	if err != nil {
		sc.log.Debug("WARN: error getting worst health dxfile:", err)
		return err
	}

	// Refresh the worker pool and get the set of hosts that are currently
	// useful for uploading
	hosts := sc.refreshHostsAndWorkers()

	// Push a min-heap of segments organized by upload progress
	sc.pushDirToSegmentHeap(dxFile.DxPath(), hosts, targetUnstuckSegments)
	sc.uploadHeap.mu.Lock()
	heapLen := sc.uploadHeap.heap.Len()
	sc.uploadHeap.mu.Unlock()
	if heapLen == 0 {
		sc.log.Debug("No segments added to the heap for repair from `%v` even through health was %v", dxFile.DxPath(), dxFile.GetHealth())
		sc.fileSystem.InitAndUpdateDirMetadata(dxFile.DxPath())
		return nil
	}
	sc.log.Info("Repairing", heapLen, "Segments from", dxFile.DxPath())

	// Work through the heap and repair files
	sc.uploadLoop(hosts)

	// Once we have worked through the heap, call bubble to update the
	// directory metadata
	sc.fileSystem.InitAndUpdateDirMetadata(dxFile.DxPath())
	return nil
}

func (sc *StorageClient) uploadAndRepairLoop() {
	err := sc.tm.Add()
	if err != nil {
		return
	}
	defer sc.tm.Done()

	for {
		select {
		case <-sc.tm.StopChan():
			return
		default:
		}

		// Wait for client online
		if !sc.blockUntilOnline() {
			return
		}

		// Check whether a repair is needed. If a repair is not needed, block
		// until there is a signal suggesting that a repair is needed. If there
		// is a new upload, a signal will be sent through the 'newUploads'
		// channel, and if the metadata updating code finds a file that needs
		// repairing, a signal is sent through the 'repairNeeded' channel.
		rootMetadata, err := sc.managedDirectoryMetadata(storage.RootDxPath())
		if err != nil {
			// If there is an error fetching the root directory metadata, sleep
			// for a bit and hope that on the next iteration, things will be better
			sc.log.Debug("WARN: error fetching filesystem root metadata:", err)
			select {
			case <-time.After(UploadAndRepairErrorSleepDuration):
			case <-sc.tm.StopChan():
				return
			}
			continue
		}

		// We are not upload or repair immediately because of enough health score
		if rootMetadata.Health >= dxfile.RepairHealthThreshold {
			// Block until a signal is received that there is more work to do.
			// A signal will be sent if new data to upload is received, or if
			// the health loop discovers that some files are not in good health.
			select {
			case <-sc.uploadHeap.newUploads:
			case <-sc.uploadHeap.repairNeeded:
			case <-sc.tm.StopChan():
				return
			}
			continue
		}

		// The necessary conditions for performing an upload and repair
		// iteration have been met - perform an upload and repair iteration.
		err = sc.doUploadAndRepair()
		if err != nil {
			// If there is an error performing an upload and repair iteration,
			// sleep for a bit and hope that on the next iteration, things will
			// be better.
			sc.log.Debug("WARN: error performing upload and repair iteration:", err)
			select {
			case <-time.After(UploadAndRepairErrorSleepDuration):
			case <-sc.tm.StopChan():
				return
			}
		}

		// TODO: This sleep is a hack to keep the CPU from spinning at 100% for
		// a brief time when all of the Segments in the directory have been added
		// to the repair loop, but the directory isn't full yet so it keeps
		// trying to add more Segments ???
		time.Sleep(20 * time.Millisecond)
	}
}
