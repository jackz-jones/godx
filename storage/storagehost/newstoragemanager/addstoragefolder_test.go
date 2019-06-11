package newstoragemanager

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestAddStorageFolderNormal test the process of adding a storagefolder
func TestAddStorageFolderNormal(t *testing.T) {
	sm := newTestStorageManager(t, "", newDisrupter())
	path := randomFolderPath(t, "")
	size := uint64(1 << 25)
	err := sm.addStorageFolder(path, size)
	if err != nil {
		t.Fatal(err)
	}
	// The folder should exist on disk
	dataFilePath := filepath.Join(path, dataFileName)
	fileInfo, err := os.Stat(dataFilePath)
	if err != nil {
		t.Fatal(err)
	}
	if expectSize := numSectorsToSize(sizeToNumSectors(size)); uint64(fileInfo.Size()) < expectSize {
		t.Fatalf("file size smaller than expected. Got %v, expect %v", fileInfo.Size(), expectSize)
	}
	// The folder should exist in memory
	if !sm.folders.exist(path) {
		t.Fatal("folder not exist in sm.folders")
	}
	sf, err := sm.folders.get(path)
	if err != nil {
		t.Fatal(err)
	}
	if sf.status != folderAvailable {
		t.Errorf("folder status")
	}
	if sf.numSectors != sizeToNumSectors(size) {
		t.Errorf("numSectors unexpected. Got %v, expect %v", sf.numSectors, sizeToNumSectors(size))
	}
	expectUsageSize := sf.numSectors / bitVectorGranularity
	if sf.numSectors%bitVectorGranularity != 0 {
		expectUsageSize++
	}
	if uint64(len(sf.usage)) != expectUsageSize {
		t.Errorf("usage size unexpected. got %v, expect %v", expectUsageSize, len(sf.usage))
	}
	// the storage folder's lock shall be released
	if locked(sf.lock) {
		t.Errorf("The storage folder still locked after update")
	}
	if locked(&sm.folders.lock) {
		t.Errorf("The folders still locked after update")
	}
	// Check the database data
	dbSf, err := sm.db.loadStorageFolder(path)
	if err != nil {
		t.Fatalf("check storage folder error: %v", err)
	}
	if dbSf.path != path {
		t.Errorf("folder stored in db not equal in path. Expect %v, got %v", path, dbSf.path)
	}
	if len(dbSf.usage) != len(sf.usage) {
		t.Errorf("folder stored in db not equal in usage size. Expect %v, got %v", len(sf.usage), len(dbSf.usage))
	}
	if dbSf.numSectors != sf.numSectors {
		t.Errorf("folder stored in db not equal in numsectors. Expect %v, got %v", sf.numSectors, dbSf.numSectors)
	}
}

// TestAddStorageFolderRecover test the recover scenario of add storage folder
func TestAddStorageFolderRecover(t *testing.T) {
	d := newDisrupter().register("mock process disrupted", func() bool {
		return true
	})
	sm := newTestStorageManager(t, "", d)
	path := randomFolderPath(t, "")
	size := uint64(1 << 25)
	if err := sm.addStorageFolder(path, size); err != nil {
		t.Fatal(err)
	}
	sm.shutdown(t, 100*time.Millisecond)
	// restart the storage manager
	newSM, err := New(sm.persistDir)
	if err != nil {
		t.Fatal(err)
	}
	if err = newSM.Start(); err != nil {
		t.Fatal(err)
	}
	// wait for 100ms for the update to complete
	<-time.After(100 * time.Millisecond)
	if sm.folders.exist(filepath.Join(path, dataFileName)) {
		t.Fatalf("folders exist path %v", path)
	}
	exist, err := newSM.db.hasStorageFolder(path)
	if err != nil {
		t.Fatalf("database check folder exist: %v", err)
	}
	if exist {
		t.Fatalf("database has folder")
	}
	if _, err := os.Stat(filepath.Join(path, dataFileName)); err == nil || !os.IsNotExist(err) {
		t.Fatalf("file exist on disk %v", filepath.Join(path, dataFileName))
	}
}

// locked checks whether the lock is unlocked or not.
func locked(lock sync.Locker) bool {
	c := make(chan struct{})
	go func() {
		lock.Lock()
		lock.Unlock()
		close(c)
	}()
	select {
	case <-c:
		return true
	default:
	}
	return false
}
