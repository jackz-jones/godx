// Copyright 2019 DxChain, All rights reserved.
// Use of this source code is governed by an Apache
// License 2.0 that can be found in the LICENSE file

package storagehostmanager

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/DxChainNetwork/godx/common"
	"github.com/DxChainNetwork/godx/common/threadmanager"
	"github.com/DxChainNetwork/godx/log"
	"github.com/DxChainNetwork/godx/p2p/enode"
	"github.com/DxChainNetwork/godx/storage"
	"github.com/DxChainNetwork/godx/storage/storageclient/storagehosttree"
)

// StorageHostManager contains necessary fields that are used to manage storage hosts
// establishing connection with them and getting their settings
type StorageHostManager struct {
	// storage client and eth backend
	b   storage.ClientBackend
	eth storage.EthBackend

	rent            storage.RentPayment
	hostEvaluator   HostEvaluator
	storageHostTree storagehosttree.StorageHostTree

	// ip violation check
	ipViolationCheck bool

	// maintenance related
	// initialScanFinished is atomic value to denote the status whether the initial scan has been
	// finished. Initialized to value 0, and changed value to 1 when initial scan is finished.
	initialScanFinished uint32
	scanWaitList        []storage.HostInfo
	scanLookup          map[enode.ID]struct{}
	scanWait            bool
	scanningWorkers     int

	// persistent directory
	persistDir string

	// utils
	log  log.Logger
	lock sync.RWMutex
	tm   threadmanager.ThreadManager

	// filter mode related
	filterMode    FilterMode
	filteredHosts map[enode.ID]struct{}
	filteredTree  storagehosttree.StorageHostTree

	// blockHeight and its lock
	blockHeight     uint64
	blockHeightLock sync.RWMutex

	// host market pricing cache
	cachedPrices cachedPrices
}

// New will initialize HostPoolManager, making the host pool stay updated
func New(persistDir string) *StorageHostManager {
	// initialization
	shm := &StorageHostManager{
		persistDir:    persistDir,
		rent:          storage.DefaultRentPayment,
		scanLookup:    make(map[enode.ID]struct{}),
		filterMode:    DisableFilter,
		filteredHosts: make(map[enode.ID]struct{}),
	}

	shm.hostEvaluator = newDefaultEvaluator(shm, shm.rent)
	shm.storageHostTree = storagehosttree.New()
	shm.filteredTree = shm.storageHostTree
	shm.log = log.New()

	shm.log.Info("Storage Host Manager Initialized")

	return shm
}

// Start will start to load prior settings, start go routines to automatically save
// the settings every 2 min, and go routine to start storage host maintenance
func (shm *StorageHostManager) Start(b storage.ClientBackend) error {
	// initialization
	shm.b = b

	// load prior settings
	err := shm.loadSettings()

	if err != nil && !os.IsNotExist(err) {
		return err
	}

	if err := shm.tm.AfterStop(func() error {
		return shm.saveSettings()
	}); err != nil {
		return err
	}

	// automatically save the settings every 2 minutes
	go shm.autoSaveSettings()

	// subscribe block chain change event
	go shm.subscribeChainChangEvent()

	// started scan and update storage host information
	go shm.scan()

	shm.log.Info("Storage Host Manager Started")

	return nil
}

// Close will send stop signal to routine manager, terminate all the
// running go routines
func (shm *StorageHostManager) Close() error {
	return shm.tm.Stop()
}

// ActiveStorageHosts will return all active storage host information
func (shm *StorageHostManager) ActiveStorageHosts() (activeStorageHosts []storage.HostInfo) {
	allHosts := shm.storageHostTree.All()
	// based on the host information, filter out active hosts
	for _, host := range allHosts {
		numScanRecords := len(host.ScanRecords)
		if numScanRecords == 0 {
			continue
		}
		if !host.ScanRecords[numScanRecords-1].Success {
			continue
		}
		if !host.AcceptingContracts {
			continue
		}
		activeStorageHosts = append(activeStorageHosts, host)
	}
	return
}

// SetRentPayment will modify the rent payment and update the host evaluations in storage host
// tree as well as filtered tree
func (shm *StorageHostManager) SetRentPayment(rent storage.RentPayment) (err error) {
	shm.lock.Lock()
	defer shm.lock.Unlock()
	// during initialization, the value might be empty
	if reflect.DeepEqual(rent, storage.RentPayment{}) {
		rent = storage.DefaultRentPayment
	}
	// update the rent
	shm.rent = rent
	// update the host evaluator
	hostEvaluator := newDefaultEvaluator(shm, rent)
	shm.hostEvaluator = hostEvaluator
	// Update the storage host tree and filtered tree
	if err = shm.evaluateHostTree(shm.storageHostTree); err != nil {
		return fmt.Errorf("cannot update the host tree: %v", err)
	}
	if err = shm.evaluateHostTree(shm.filteredTree); err != nil {
		return fmt.Errorf("cannot update the filtered host tree: %v", err)
	}
	return nil
}

// evaluateHostTrees evaluate all nodes in host tree and update
func (shm *StorageHostManager) evaluateHostTree(tree storagehosttree.StorageHostTree) (err error) {
	nodes := tree.All()
	for _, hi := range nodes {
		eval := shm.hostEvaluator.Evaluate(hi)
		err := tree.HostInfoUpdate(hi, eval)
		if err == storagehosttree.ErrHostNotExists {
			// If error is storagehosttree.ErrHostNotExists, meaning the host node is removed during
			// the update by some other goroutines. Ignore the error and continue to next loop
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// RetrieveRentPayment will return the current rent payment settings for storage host manager
func (shm *StorageHostManager) RetrieveRentPayment() (rent storage.RentPayment) {
	shm.lock.RLock()
	defer shm.lock.RUnlock()

	return shm.rent
}

// RetrieveHostInfo will acquire the storage host information based on the enode ID provided.
// Before returning the storage host information, the settings will be validated first
func (shm *StorageHostManager) RetrieveHostInfo(id enode.ID) (hi storage.HostInfo, exists bool) {
	// get the storage host information
	if hi, exists = shm.storageHostTree.RetrieveHostInfo(id); !exists {
		return
	}

	// check if the storage host should be filtered
	// if WhitelistFilter and the host is stored inside the filtered host, meaning not filtered
	// if WhitelistFilter but host is not stored in the filtered host, FILTERED, the storage client
	// cannot sign contract with it
	shm.lock.RLock()
	whitelist := shm.filterMode == WhitelistFilter
	filteredHosts := shm.filteredHosts
	_, exist := filteredHosts[hi.EnodeID]
	shm.lock.RUnlock()

	// update host historical interaction record before returning
	shm.lock.Lock()
	hi.Filtered = whitelist != exist
	shm.lock.Unlock()

	return
}

// SetIPViolationCheck will set the ipViolationCheck to be true. For storage hosts
// who are located in the same network, they will be marked as bad storage hosts
func (shm *StorageHostManager) SetIPViolationCheck(violationCheck bool) {
	shm.lock.Lock()
	defer shm.lock.Unlock()
	shm.ipViolationCheck = violationCheck
}

// RetrieveIPViolationCheckSetting will return the current tipViolationCheck
func (shm *StorageHostManager) RetrieveIPViolationCheckSetting() (violationCheck bool) {
	shm.lock.RLock()
	defer shm.lock.RUnlock()
	return shm.ipViolationCheck
}

// FilterIPViolationHosts will evaluate the storage hosts passed in. For hosts located under the same
// network, it will be considered as badHosts if the IPViolation is enabled
func (shm *StorageHostManager) FilterIPViolationHosts(hostIDs []enode.ID) (badHostIDs []enode.ID) {
	shm.lock.RLock()
	defer shm.lock.RUnlock()

	// check if the ipViolationCheck is enabled
	if !shm.ipViolationCheck {
		return
	}

	var hostsInfo []storage.HostInfo

	// hosts validation
	for _, id := range hostIDs {
		hi, exists := shm.storageHostTree.RetrieveHostInfo(id)
		if !exists {
			badHostIDs = append(badHostIDs, id)
			continue
		}
		hostsInfo = append(hostsInfo, hi)
	}

	// sort the information based on the LastIPChange time. When there are two storage hosts
	// with same network address. The one that changes the IP earliest will not be filtered
	// out
	sort.Slice(hostsInfo[:], func(i, j int) bool {
		return hostsInfo[i].LastIPNetWorkChange.Before(hostsInfo[j].LastIPNetWorkChange)
	})

	// start the filter
	ipFilter := storagehosttree.NewFilter()
	for _, hi := range hostsInfo {
		if ipFilter.Filtered(hi.IP) {
			badHostIDs = append(badHostIDs, hi.EnodeID)
			continue
		}
		ipFilter.Add(hi.IP)
	}

	return
}

// RetrieveRandomHosts will randomly select storage hosts from the storage host pool
//  1. blacklist represents the storage host that are prohibited to be selected
//  2. addrBlacklist represents for any storage host whose network address is caontine
func (shm *StorageHostManager) RetrieveRandomHosts(num int, blacklist, addrBlacklist []enode.ID) (infos []storage.HostInfo, err error) {
	shm.lock.RLock()
	ipCheck := shm.ipViolationCheck
	shm.lock.RUnlock()

	// if the initialize scan is not complete
	if !shm.isInitialScanFinished() {
		err = errors.New("storage host pool initial scan is not finished")
		return
	}

	// select random
	if ipCheck {
		infos = shm.filteredTree.SelectRandom(num, blacklist, addrBlacklist)
	} else {
		infos = shm.filteredTree.SelectRandom(num, blacklist, nil)
	}

	return
}

// Evaluate will calculate and return the evaluation of a single storage host
func (shm *StorageHostManager) Evaluate(host storage.HostInfo) int64 {
	return shm.hostEvaluator.Evaluate(host)
}

// AllHosts will return all available storage hosts
func (shm *StorageHostManager) AllHosts() []storage.HostInfo {
	shm.lock.RLock()
	defer shm.lock.RUnlock()
	return shm.storageHostTree.All()
}

// StorageHostRanks will return the storage host rankings based on their evaluations. The
// higher the evaluation is, the higher order it will be placed
func (shm *StorageHostManager) StorageHostRanks() (rankings []StorageHostRank) {
	shm.lock.RLock()
	defer shm.lock.RUnlock()

	allHosts := shm.storageHostTree.All()
	// based on the host information, calculate the evaluation
	for _, host := range allHosts {
		evalDetail := shm.hostEvaluator.EvaluateDetail(host)

		rankings = append(rankings, StorageHostRank{
			EvaluationDetail: evalDetail,
			EnodeID:          host.EnodeID.String(),
		})
	}
	return
}

// insert will insert host information into the storageHostTree
func (shm *StorageHostManager) insert(hi storage.HostInfo) error {
	// evaluate the host info
	eval := shm.hostEvaluator.Evaluate(hi)
	// insert the host information into the storage host tree
	err := shm.storageHostTree.Insert(hi, eval)

	// check if the host information contained in the filtered host
	shm.lock.RLock()
	_, exists := shm.filteredHosts[hi.EnodeID]
	shm.lock.RUnlock()

	// if the filter mode is the whitelist, add the one into filtered host tree
	if exists && shm.filterMode == WhitelistFilter {
		errF := shm.filteredTree.Insert(hi, eval)
		if errF != nil && errF != storagehosttree.ErrHostExists {
			err = common.ErrCompose(err, errF)
		}
	}
	return err
}

// remove will remove the host information from the storageHostTree
func (shm *StorageHostManager) remove(enodeid enode.ID) error {
	err := shm.storageHostTree.Remove(enodeid)
	_, exists := shm.filteredHosts[enodeid]

	if exists && shm.filterMode == WhitelistFilter {
		errF := shm.filteredTree.Remove(enodeid)
		if errF != nil && errF != storagehosttree.ErrHostNotExists {
			err = common.ErrCompose(err, errF)
		}
	}
	return err
}

// modify will modify the host information from the StorageHostTree
func (shm *StorageHostManager) modify(hi storage.HostInfo) error {
	// Evaluate the host info and update
	eval := shm.hostEvaluator.Evaluate(hi)
	err := shm.storageHostTree.HostInfoUpdate(hi, eval)

	_, exists := shm.filteredHosts[hi.EnodeID]

	if exists && shm.filterMode == WhitelistFilter {
		errF := shm.filteredTree.HostInfoUpdate(hi, eval)
		if errF != nil && errF != storagehosttree.ErrHostNotExists {
			err = common.ErrCompose(err, errF)
		}
	}
	return err
}

// getBlockHeight get the current block number from storage host manager
func (shm *StorageHostManager) getBlockHeight() uint64 {
	shm.blockHeightLock.RLock()
	defer shm.blockHeightLock.RUnlock()

	return shm.blockHeight
}

// setBlockHeight set storage host manager's block number to the target val
func (shm *StorageHostManager) setBlockHeight(val uint64) {
	shm.blockHeightLock.Lock()
	defer shm.blockHeightLock.Unlock()

	shm.blockHeight = val
}

// incrementBlockHeight increment the block height by 1
func (shm *StorageHostManager) incrementBlockHeight() {
	shm.blockHeightLock.Lock()
	defer shm.blockHeightLock.Unlock()

	shm.blockHeight++
}

// decrementBlockHeight decrement the block height by 1
func (shm *StorageHostManager) decrementBlockHeight() {
	shm.blockHeightLock.Lock()
	defer shm.blockHeightLock.Unlock()

	shm.blockHeight--
}

// isInitialScanFinished return whether the initial scan has been finished
func (shm *StorageHostManager) isInitialScanFinished() bool {
	return atomic.LoadUint32(&(shm.initialScanFinished)) == 1
}

// finishInitialScan denote the initial scan is already finished
func (shm *StorageHostManager) finishInitialScan() {
	atomic.StoreUint32(&(shm.initialScanFinished), 1)
}
