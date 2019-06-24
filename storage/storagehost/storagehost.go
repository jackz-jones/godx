// Copyright 2019 DxChain, All rights reserved.
// Use of this source code is governed by an Apache
// License 2.0 that can be found in the LICENSE file.

package storagehost

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/DxChainNetwork/godx/accounts"
	"github.com/DxChainNetwork/godx/common"
	tm "github.com/DxChainNetwork/godx/common/threadmanager"
	"github.com/DxChainNetwork/godx/ethdb"
	"github.com/DxChainNetwork/godx/log"
	"github.com/DxChainNetwork/godx/storage"
	sm "github.com/DxChainNetwork/godx/storage/storagehost/storagemanager"
)

// StorageHost provide functions for storageHost management
// It loads or use default config when it have been initialized
// It aims at communicate by protocal with client and lent its own storage to the client
type StorageHost struct {
	// backend support
	ethBackend storage.EthBackend
	parseAPI   storage.ParsedAPI

	// Account manager for wallet/account related operation
	am *accounts.Manager

	// storageHost basic config
	blockHeight      uint64
	config           storage.HostIntConfig
	financialMetrics HostFinancialMetrics

	// storage host manager for manipulating the file storage system
	sm.StorageManager

	lockedStorageResponsibility map[common.Hash]*TryMutex

	// things for log and persistence
	db         *ethdb.LDBDatabase
	persistDir string
	log        log.Logger

	// things for thread safety
	lock sync.RWMutex
	tm   tm.ThreadManager
}

// New Initialize the Host, including init the structure
// load or use the default config, init db and ext.
func New(persistDir string) (*StorageHost, error) {
	// do a host creation, but incomplete config
	h := StorageHost{
		log:                         log.New(),
		persistDir:                  persistDir,
		lockedStorageResponsibility: make(map[common.Hash]*TryMutex),
	}

	var err error
	// Create the data path
	if err = os.MkdirAll(h.persistDir, 0700); err != nil {
		return nil, err
	}
	// Create the database

	// initialize the storage manager
	if h.StorageManager, err = sm.New(persistDir); err != nil {
		return nil, err
	}
	// open the database
	if h.db, err = openDB(filepath.Join(persistDir, databaseFile)); err != nil {
		return nil, err
	}

	return &h, nil
}

// Start loads all APIs and make them mapping, also introduce the account
// manager as a member variable in side the StorageHost
func (h *StorageHost) Start(eth storage.EthBackend) (err error) {
	// init the account manager
	h.am = eth.AccountManager()
	h.ethBackend = eth

	// load the data from file or from default config
	if err = h.load(); err != nil {
		return err
	}
	// start the storage manager
	if err = h.StorageManager.Start(); err != nil {
		return err
	}
	// parse storage contract tx API
	err = storage.FilterAPIs(h.ethBackend.APIs(), &h.parseAPI)
	if err != nil {
		h.log.Error("responsibilityFailed to parse storage contract tx API for host", "error", err)
		return
	}
	//Delete residual storage responsibility
	if err = h.pruneStaleStorageResponsibilities(); err != nil {
		return err
	}
	fmt.Println("after prune")
	// subscribe block chain change event
	go h.subscribeChainChangEvent()
	return nil
}

// Close the storage host and persist the data
func (h *StorageHost) Close() error {
	err := h.tm.Stop()

	newErr := h.StorageManager.Close()
	err = common.ErrCompose(err, newErr)

	newErr = h.syncConfig()
	err = common.ErrCompose(err, newErr)
	return err
}

// load do the following things:
// 1. load the config from file
// 2. if the config file not found, create the config file, and use the default config
// 3.  synchronize the data to config file
func (h *StorageHost) load() error {
	h.lock.Lock()
	defer h.lock.Unlock()

	// try to load from the config files,
	if err := h.loadConfig(); err == nil {
		return err
	} else if !os.IsNotExist(err) {
		// if the error is NOT caused by FILE NOT FOUND Exception
		return err
	}

	// At this step, the error is caused by FILE NOT FOUND Exception
	// Create the config file
	h.log.Info("Creat a new HostSetting file")

	// currently the error is caused by file not found exception
	// create the file
	file, err := os.Create(filepath.Join(h.persistDir, HostSettingFile))
	if err != nil {
		// if the error is throw when create the file
		// close the file and directly return the error
		_ = file.Close()
		return err
	}
	// assert the error is nil, close the file
	if err := file.Close(); err != nil {
		return err
	}
	// load the default config
	h.config = defaultConfig()

	// and get synchronization
	if syncErr := h.syncConfig(); syncErr != nil {
		h.log.Warn("Tempt to synchronize config to file responsibilityFailed: " + syncErr.Error())
	}
	return nil
}

// getPaymentAddress get the current payment address. If no address is set, assign the first
// account address as the payment address
func (h *StorageHost) getPaymentAddress() (common.Address, error) {
	h.lock.Lock()
	h.lock.Unlock()

	paymentAddress := h.config.PaymentAddress
	if paymentAddress != (common.Address{}) {
		return paymentAddress, nil
	}
	//Local node does not contain wallet
	if wallets := h.ethBackend.AccountManager().Wallets(); len(wallets) > 0 {
		//The local node does not have any wallet address yet
		if accs := wallets[0].Accounts(); len(accs) > 0 {
			paymentAddress := accs[0].Address
			//the first address in the local wallet will be used as the paymentAddress by default.
			h.config.PaymentAddress = paymentAddress
			if err := h.syncConfig(); err != nil {
				return common.Address{}, fmt.Errorf("cannot save host config: %v", err)
			}
			return paymentAddress, nil
		}
	}
	return common.Address{}, errors.New("no wallet accounts available")
}

// getExternalConfig return the host external config, which configure host through,
// user should not able to modify the config
func (h *StorageHost) getExternalConfig() storage.HostExtConfig {
	h.lock.Lock()
	defer h.lock.Unlock()

	// mock the return of host external config
	return h.externalConfig()
}

// getInternalConfig Return the internal config of host
func (h *StorageHost) getInternalConfig() storage.HostIntConfig {
	h.lock.Lock()
	defer h.lock.Unlock()

	return h.config
}

// getFinancialMetrics contains the information about the activities,
// commitments, rewards of host
func (h *StorageHost) getFinancialMetrics() HostFinancialMetrics {
	h.lock.RLock()
	defer h.lock.RUnlock()

	return h.financialMetrics
}

// print the persist directory of the host
func (h *StorageHost) getPersistDir() string {
	h.lock.RLock()
	defer h.lock.RUnlock()

	return h.persistDir
}

// setAcceptContracts set the HostIntConfig.AcceptingContracts to value
func (h *StorageHost) setAcceptContracts(val bool) error {
	h.lock.Lock()
	defer h.lock.Unlock()

	h.config.AcceptingContracts = val
	return h.syncConfig()
}

// setMaxDownloadBatch set the MaxDownloadBatchSize
func (h *StorageHost) setMaxDownloadBatchSize(val uint64) error {
	h.lock.Lock()
	defer h.lock.Unlock()

	h.config.MaxDownloadBatchSize = val
	return h.syncConfig()
}

// setMaxDuration set the MaxDuration
func (h *StorageHost) setMaxDuration(val uint64) error {
	h.lock.Lock()
	defer h.lock.Unlock()

	h.config.MaxDuration = val
	return h.syncConfig()
}

// setMaxReviseBatchSize set the MaxReviseBatchSize
func (h *StorageHost) setMaxReviseBatchSize(val uint64) error {
	h.lock.Lock()
	defer h.lock.Unlock()

	h.config.MaxReviseBatchSize = val
	return h.syncConfig()
}

// setWindowSize set the WindowSize
func (h *StorageHost) setWindowSize(val uint64) error {
	h.lock.Lock()
	defer h.lock.Unlock()

	h.config.WindowSize = val
	return h.syncConfig()
}

// setPaymentAddress set the account to the address
func (h *StorageHost) setPaymentAddress(addr common.Address) error {
	account := accounts.Account{Address: addr}
	_, err := h.ethBackend.AccountManager().Find(account)
	if err != nil {
		return errors.New("unknown account")
	}
	h.lock.Lock()
	defer h.lock.Unlock()
	h.config.PaymentAddress = addr

	return h.syncConfig()
}

// setDeposit set the deposit to val
func (h *StorageHost) setDeposit(val common.BigInt) error {
	h.lock.Lock()
	defer h.lock.Unlock()

	h.config.Deposit = val
	return h.syncConfig()
}

// setDepositBudget set the DepositBudget to val
func (h *StorageHost) setDepositBudget(val common.BigInt) error {
	h.lock.Lock()
	defer h.lock.Unlock()

	h.config.DepositBudget = val
	return h.syncConfig()
}

// setMaxDeposit set the MaxDeposit to val
func (h *StorageHost) setMaxDeposit(val common.BigInt) error {
	h.lock.Lock()
	defer h.lock.Unlock()

	h.config.MaxDeposit = val
	return h.syncConfig()
}

// setMinBaseRPCPrice set the MinBaseRPCPrice to val
func (h *StorageHost) setMinBaseRPCPrice(val common.BigInt) error {
	h.lock.Lock()
	defer h.lock.Unlock()

	h.config.MinBaseRPCPrice = val
	return h.syncConfig()
}

// setMinContractPrice set the MinContractPrice to val
func (h *StorageHost) setMinContractPrice(val common.BigInt) error {
	h.lock.Lock()
	defer h.lock.Unlock()

	h.config.MinContractPrice = val
	return h.syncConfig()
}

// setMinDownloadBandwidthPrice set the MinDownloadBandwidthPrice to val
func (h *StorageHost) setMinDownloadBandwidthPrice(val common.BigInt) error {
	h.lock.Lock()
	defer h.lock.Unlock()

	h.config.MinDownloadBandwidthPrice = val
	return h.syncConfig()
}

// setMinSectorAccessPrice set the MinSectorAccessPrice to val
func (h *StorageHost) setMinSectorAccessPrice(val common.BigInt) error {
	h.lock.Lock()
	defer h.lock.Unlock()

	h.config.MinSectorAccessPrice = val
	return h.syncConfig()
}

// setMinStoragePrice set the MinStoragePrice to val
func (h *StorageHost) setMinStoragePrice(val common.BigInt) error {
	h.lock.Lock()
	defer h.lock.Unlock()

	h.config.MinStoragePrice = val
	return h.syncConfig()
}

// setMinUploadBandwidthPrice set the MinUploadBandwidthPrice to val
func (h *StorageHost) setMinUploadBandwidthPrice(val common.BigInt) error {
	h.lock.Lock()
	defer h.lock.Unlock()

	h.config.MinUploadBandwidthPrice = val
	return h.syncConfig()
}
