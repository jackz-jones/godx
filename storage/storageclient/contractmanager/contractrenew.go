// Copyright 2019 DxChain, All rights reserved.
// Use of this source code is governed by an Apache
// License 2.0 that can be found in the LICENSE file

package contractmanager

import (
	"crypto/ecdsa"
	"errors"
	"fmt"
	"math/big"

	"github.com/DxChainNetwork/godx/accounts"
	"github.com/DxChainNetwork/godx/common"
	"github.com/DxChainNetwork/godx/common/math"
	"github.com/DxChainNetwork/godx/core/types"
	"github.com/DxChainNetwork/godx/crypto"
	"github.com/DxChainNetwork/godx/p2p/enode"
	"github.com/DxChainNetwork/godx/rlp"
	"github.com/DxChainNetwork/godx/storage"
	"github.com/DxChainNetwork/godx/storage/storageclient/contractset"
	"github.com/DxChainNetwork/godx/storage/storageclient/proto"
	"github.com/DxChainNetwork/godx/storage/storagehost"
)

var (
	zeroValue = new(big.Int).SetInt64(0)

	extraRatio = 0.02
)

// checkForContractRenew will loop through all active contracts and filter out those needs to be renewed.
// There are two types of contract needs to be renewed
// 		1. contracts that are about to expired. they need to be renewed
// 		2. contracts that have insufficient amount of funding, meaning the contract is about to be
// 		   marked as not good for data uploading
func (cm *ContractManager) checkForContractRenew() (closeToExpireRenews []contractRenewRecord, insufficientFundingRenews []contractRenewRecord) {
	cm.lock.RLock()
	currentBlockHeight := cm.blockHeight
	rentPayment := cm.rentPayment
	cm.lock.RUnlock()

	// loop through all active contracts, get the priorityRenews and emptyRenews
	for _, contract := range cm.activeContracts.RetrieveAllContractsMetaData() {
		// validate the storage host for the contract, check if the host exists or get filtered
		host, exists := cm.hostManager.RetrieveHostInfo(contract.EnodeID)
		if !exists || host.Filtered {
			continue
		}

		// verify if the contract is good for renew
		if !contract.Status.RenewAbility {
			continue
		}

		// for contract that is about to expire, it will be added to the priorityRenews
		// calculate the renewCostEstimation and update the priorityRenews
		if currentBlockHeight+rentPayment.RenewWindow >= contract.EndHeight {
			estimateContractRenewCost := cm.renewCostEstimation(host, contract, currentBlockHeight, rentPayment)
			closeToExpireRenews = append(closeToExpireRenews, contractRenewRecord{
				id:   contract.ID,
				cost: estimateContractRenewCost,
			})
			continue
		}

		// for those contracts has insufficient funding, they should be renewed because otherwise
		// after a while, they will be marked as not good for upload
		sectorStorageCost := host.StoragePrice.MultUint64(contractset.SectorSize * rentPayment.Period)
		sectorUploadBandwidthCost := host.UploadBandwidthPrice.MultUint64(contractset.SectorSize)
		sectorDownloadBandwidthCost := host.DownloadBandwidthPrice.MultUint64(contractset.SectorSize)
		totalSectorCost := sectorUploadBandwidthCost.Add(sectorDownloadBandwidthCost).Add(sectorStorageCost)

		remainingBalancePercentage := contract.ClientBalance.Div(contract.TotalCost).Float64()

		if contract.ClientBalance.Cmp(totalSectorCost.MultUint64(3)) < 0 || remainingBalancePercentage < minContractPaymentRenewalThreshold {
			insufficientFundingRenews = append(insufficientFundingRenews, contractRenewRecord{
				id:   contract.ID,
				cost: contract.TotalCost.MultUint64(2),
			})
		}
	}

	return
}

// resetFailedRenews will update the failedRenewCount list, which only includes the failedRenewCount
// in the current renew lists
func (cm *ContractManager) resetFailedRenews(closeToExpireRenews []contractRenewRecord, insufficientFundingRenews []contractRenewRecord) {
	cm.lock.Lock()
	filteredFailedRenews := make(map[storage.ContractID]uint64)

	// loop through the closeToExpireRenews, get the failedRenewCount
	for _, renewRecord := range closeToExpireRenews {
		contractID := renewRecord.id
		if _, exists := cm.failedRenewCount[contractID]; exists {
			filteredFailedRenews[contractID] = cm.failedRenewCount[contractID]
		}
	}

	// loop through the insufficientFundingRenews, get the failed renews
	for _, renewRecord := range insufficientFundingRenews {
		contractID := renewRecord.id
		if _, exists := cm.failedRenewCount[contractID]; exists {
			filteredFailedRenews[contractID] = cm.failedRenewCount[contractID]
		}
	}

	// reset the failedRenewCount
	cm.failedRenewCount = filteredFailedRenews
	cm.lock.Unlock()
}

// prepareContractRenew will loop through all record in the renewRecords and start to renew
// each contract. Before contract renewing get started, the fund will be validated first.
func (cm *ContractManager) prepareContractRenew(renewRecords []contractRenewRecord, clientRemainingFund common.BigInt) (remainingFund common.BigInt, terminate bool) {
	// get the data needed
	cm.lock.RLock()
	rentPayment := cm.rentPayment
	blockHeight := cm.blockHeight
	currentPeriod := cm.currentPeriod
	contractEndHeight := cm.currentPeriod + cm.rentPayment.Period + cm.rentPayment.RenewWindow
	cm.lock.RUnlock()

	// loop through all contracts that need to be renewed, and prepare to renew the contract
	for _, record := range renewRecords {
		// verify that the cost needed for contract renew does not exceed the clientRemainingFund
		if clientRemainingFund.Cmp(record.cost) < 0 {
			continue
		}

		// renew the contract, get the spending for the renew
		renewCost, err := cm.contractRenewStart(record, currentPeriod, rentPayment, blockHeight, contractEndHeight)
		if err != nil {
			cm.log.Error(fmt.Sprintf("contract renew failed. id: %v, err : %v", record.id, err.Error()))
		}

		// update the remaining fund
		remainingFund = clientRemainingFund.Sub(renewCost)

		// check maintenance termination
		if terminate = cm.checkMaintenanceTermination(); terminate {
			return
		}
	}

	return
}

// contractRenewStart will start to perform contract renew operation
// 		1. before contract renew, validate the contract first
// 		2. renew the contract
// 		3. if the renew failed, handle the failed situation
//   	4. otherwise, update the contract manager
func (cm *ContractManager) contractRenewStart(record contractRenewRecord, currentPeriod uint64, rentPayment storage.RentPayment, blockHeight uint64, contractEndHeight uint64) (renewCost common.BigInt, err error) {
	// get the information needed
	renewContractID := record.id
	renewContractCost := record.cost

	// mark the contract as renewing, and remove it at the end
	cm.lock.Lock()
	cm.renewing[renewContractID] = true
	cm.lock.Unlock()

	defer func() {
		cm.lock.Lock()
		delete(cm.renewing, renewContractID)
		cm.lock.Unlock()
	}()

	// TODO (mzhang): making sure that the contract will not be revised while renewing it, waiting for HZ

	// acquire the contract
	contract, exists := cm.activeContracts.Acquire(renewContractID)
	if !exists {
		renewCost = common.BigInt0
		err = fmt.Errorf("the contract that is trying to be renewed with id %v no longer exists", renewContractID)
		return
	}

	// 1. get the contract status and check its renewAbility, contract validation
	stats, exists := cm.retrieveContractStatus(renewContractID)
	if !exists || !stats.RenewAbility {
		if err := cm.activeContracts.Return(contract); err != nil {
			cm.log.Warn("during the contract renew process, the contract cannot be returned because it has been deleted already")
		}
		renewCost = common.BigInt0
		err = fmt.Errorf("contract with id %v is marked as unable to be renewed", renewContractID)
		return
	}

	// 2. contract renew
	renewedContract, renewErr := cm.renew()

	// 3. handle the failed renews
	if renewErr != nil {
		if err := cm.activeContracts.Return(contract); err != nil {
			cm.log.Warn("during the handle contract renew failed process, the contract cannot be returned because it has been deleted already")
		}
		renewCost = common.BigInt0
		err = cm.handleRenewFailed(contract, renewErr, rentPayment, blockHeight, stats)
		return
	}

	// 4. update the contract manager
	renewedContractStatus := storage.ContractStatus{
		UploadAbility: true,
		RenewAbility:  true,
		Canceled:      false,
	}
	if err := cm.updateContractStatus(renewedContract.ID, renewedContractStatus); err != nil {
		// renew succeed, but status update failed
		cm.log.Warn(fmt.Sprintf("failed to update the renewed contract status: %s", err.Error()))
		if err := cm.activeContracts.Return(contract); err != nil {
			cm.log.Warn("during the updating renewed contract failed process, the contract cannot be returned because it has been deleted already")
			renewCost = renewContractCost
			err = nil
			return
		}
	}

	// update the old contract status
	stats.RenewAbility = false
	stats.UploadAbility = false
	stats.Canceled = true
	if err := contract.UpdateStatus(stats); err != nil {
		cm.log.Warn("failed to update the old contract (before renew) status")
		if err := cm.activeContracts.Return(contract); err != nil {
			cm.log.Warn("during the old contract status update process, the contract cannot be returned because it has been deleted already")
			renewCost = renewContractCost
			err = nil
			return
		}
	}

	// update the renewedFrom, renewedTo, expiredContract field
	cm.lock.Lock()
	cm.renewedFrom[renewedContract.ID] = contract.Metadata().ID
	cm.renewedTo[contract.Metadata().ID] = renewedContract.ID
	cm.expiredContracts[contract.Metadata().ID] = contract.Metadata()
	cm.lock.Unlock()

	// save the information persistently
	if err := cm.saveSettings(); err != nil {
		cm.log.Warn(fmt.Sprintf("failed to seave the settings persistently during the contract renew start: %s", err.Error()))
	}

	// delete the old contract from the active contract list
	if err := cm.activeContracts.Delete(contract); err != nil {
		cm.log.Warn("failed to delete the cotnract from the active contract list after renew: %s", err.Error())
	}

	renewCost = renewContractCost
	err = nil
	return
}

// handleRenewFailed will handle the failed contract renews. If the total amount of contract
func (cm *ContractManager) handleRenewFailed(failedContract *contractset.Contract, renewError error, rentPayment storage.RentPayment, blockHeight uint64, contractStatus storage.ContractStatus) (err error) {
	// TODO (mzhang): check if the fail is caused by the storage host, waiting for HZ

	// get the number of failed renews, to check if the contract needs to be replaced
	cm.lock.RLock()
	numFailed, _ := cm.failedRenewCount[failedContract.Metadata().ID]
	cm.lock.RUnlock()

	secondHalfRenewWindow := blockHeight+rentPayment.RenewWindow/2 >= failedContract.Metadata().EndHeight
	contractReplace := numFailed >= consecutiveRenewFailsBeforeReplacement

	// if the contract has been failed before, passed the second half renew window, and need replacement
	// mark the contract that is trying to be renewed as canceled
	if secondHalfRenewWindow && contractReplace {
		contractStatus.UploadAbility = false
		contractStatus.RenewAbility = false
		contractStatus.Canceled = true
		if err := failedContract.UpdateStatus(contractStatus); err != nil {
			cm.log.Warn(fmt.Sprintf("failed to update the contract status during renew failed handling: %s", err.Error()))
		}

		err = fmt.Errorf("marked the contract %v as canceled due to the large amount of renew fails: %s", failedContract.Metadata().ID, renewError.Error())
		return
	}

	// otherwise, do nothing, a renew attempt to the same contract will be performed again
	err = fmt.Errorf("failed to renew the contract: %s", renewError.Error())
	return
}

func (cm *ContractManager) renew() (contract storage.ContractMetaData, err error) {
	// TODO (mzhang): WIP
	return
}

func (cm *ContractManager) ContractRenew(oldContract *contractset.Contract, params proto.ContractParams) (storage.ContractMetaData, error) {

	contract := oldContract.Header()

	lastRev := contract.GetLatestContractRevision()

	// Extract vars from params, for convenience
	allowance, funding, clientPublicKey, startHeight, endHeight, host := params.Allowance, params.Funding, params.ClientPublicKey, params.StartHeight, params.EndHeight, params.Host

	var basePrice, baseCollateral *big.Int
	if endHeight+host.WindowSize > lastRev.NewWindowEnd {
		timeExtension := uint64((endHeight + host.WindowSize) - lastRev.NewWindowEnd)
		basePrice = new(big.Int).Mul(host.StoragePrice, new(big.Int).SetUint64(lastRev.NewFileSize))
		basePrice = new(big.Int).Mul(basePrice, new(big.Int).SetUint64(timeExtension))
		// cost of already uploaded data that needs to be covered by the renewed contract.
		baseCollateral = new(big.Int).Mul(host.Collateral, new(big.Int).SetUint64(lastRev.NewFileSize))
		baseCollateral = new(big.Int).Mul(baseCollateral, new(big.Int).SetUint64(timeExtension))
		// same as basePrice.
	}

	// Calculate the payouts for the client, host, and whole contract
	period := endHeight - startHeight
	expectedStorage := allowance.ExpectedStorage / allowance.Hosts
	clientPayout, hostPayout, hostCollateral, err := ClientPayoutsPreTax(host, funding, basePrice, baseCollateral, period, expectedStorage)
	if err != nil {
		return storage.ContractMetaData{}, err
	}

	// check for negative currency
	if hostCollateral.Cmp(baseCollateral) < 0 {
		baseCollateral = hostCollateral
	}

	clientAddr := crypto.PubkeyToAddress(clientPublicKey)
	hostAddr := crypto.PubkeyToAddress(host.PublicKey)
	var hostMiss *big.Int
	hostMiss = new(big.Int).Sub(hostCollateral, baseCollateral)
	hostMiss = new(big.Int).Add(hostMiss, host.ContractPrice)
	// Create storage contract
	storageContract := types.StorageContract{
		FileSize:         lastRev.NewFileSize,
		FileMerkleRoot:   lastRev.NewFileMerkleRoot, // no proof possible without data
		WindowStart:      endHeight,
		WindowEnd:        endHeight + host.WindowSize,
		ClientCollateral: types.DxcoinCollateral{DxcoinCharge: types.DxcoinCharge{Value: clientPayout}},
		HostCollateral:   types.DxcoinCollateral{DxcoinCharge: types.DxcoinCharge{Value: hostPayout}},
		UnlockHash:       lastRev.NewUnlockHash,
		RevisionNumber:   0,
		ValidProofOutputs: []types.DxcoinCharge{
			// Deposit is returned to client
			{Value: clientPayout, Address: clientAddr},
			// Deposit is returned to host
			{Value: hostPayout, Address: hostAddr},
		},
		MissedProofOutputs: []types.DxcoinCharge{
			{Value: clientPayout, Address: clientAddr},
			{Value: hostMiss, Address: hostAddr},
		},
	}

	// Increase Successful/Failed interactions accordingly
	defer func() {
		if err != nil {
			cm.hostManager.IncrementFailedInteractions(contract.EnodeID)
		} else {
			cm.hostManager.IncrementSuccessfulInteractions(contract.EnodeID)
		}
	}()

	account := accounts.Account{Address: clientAddr}
	wallet, err := cm.b.AccountManager().Find(account)
	if err != nil {
		return storage.ContractMetaData{}, storagehost.ExtendErr("find client account error", err)
	}

	// Setup connection with storage host
	session, err := cm.b.SetupConnection(host.NetAddress)
	if err != nil {
		return storage.ContractMetaData{}, storagehost.ExtendErr("setup connection with host failed", err)
	}
	defer cm.b.Disconnect(session, host.NetAddress)

	clientContractSign, err := wallet.SignHash(account, storageContract.RLPHash().Bytes())
	if err != nil {
		return storage.ContractMetaData{}, storagehost.ExtendErr("contract sign by client failed", err)
	}

	// Send the ContractCreate request
	req := storage.ContractCreateRequest{
		StorageContract: storageContract,
		Sign:            clientContractSign,
	}

	if err := session.SendStorageContractCreation(req); err != nil {
		return storage.ContractMetaData{}, err
	}

	var hostSign []byte
	msg, err := session.ReadMsg()
	if err != nil {
		return storage.ContractMetaData{}, err
	}

	// if host send some negotiation error, client should handler it
	if msg.Code == storage.NegotiationErrorMsg {
		var negotiationErr error
		msg.Decode(&negotiationErr)
		return storage.ContractMetaData{}, negotiationErr
	}

	if err := msg.Decode(&hostSign); err != nil {
		return storage.ContractMetaData{}, err
	}

	storageContract.Signatures[0] = clientContractSign
	storageContract.Signatures[1] = hostSign

	// Assemble init revision and sign it
	storageContractRevision := types.StorageContractRevision{
		ParentID:              storageContract.RLPHash(),
		UnlockConditions:      lastRev.UnlockConditions,
		NewRevisionNumber:     1,
		NewFileSize:           storageContract.FileSize,
		NewFileMerkleRoot:     storageContract.FileMerkleRoot,
		NewWindowStart:        storageContract.WindowStart,
		NewWindowEnd:          storageContract.WindowEnd,
		NewValidProofOutputs:  storageContract.ValidProofOutputs,
		NewMissedProofOutputs: storageContract.MissedProofOutputs,
		NewUnlockHash:         storageContract.UnlockHash,
	}

	clientRevisionSign, err := wallet.SignHash(account, storageContractRevision.RLPHash().Bytes())
	if err != nil {
		return storage.ContractMetaData{}, storagehost.ExtendErr("client sign revision error", err)
	}
	storageContractRevision.Signatures = [][]byte{clientRevisionSign}

	if err := session.SendStorageContractCreationClientRevisionSign(clientRevisionSign); err != nil {
		return storage.ContractMetaData{}, storagehost.ExtendErr("send revision sign by client error", err)
	}

	var hostRevisionSign []byte
	msg, err = session.ReadMsg()
	if err != nil {
		return storage.ContractMetaData{}, err
	}

	// if host send some negotiation error, client should handler it
	if msg.Code == storage.NegotiationErrorMsg {
		var negotiationErr error
		msg.Decode(&negotiationErr)
		return storage.ContractMetaData{}, negotiationErr
	}

	if err := msg.Decode(&hostRevisionSign); err != nil {
		return storage.ContractMetaData{}, err
	}

	scBytes, err := rlp.EncodeToBytes(storageContract)
	if err != nil {
		return storage.ContractMetaData{}, err
	}

	if _, err := storage.SendFormContractTX(cm.b, clientAddr, scBytes); err != nil {
		return storage.ContractMetaData{}, storagehost.ExtendErr("Send storage contract transaction error", err)
	}

	// wrap some information about this contract
	header := contractset.ContractHeader{
		ID:                     storage.ContractID(storageContract.ID()),
		EnodeID:                PubkeyToEnodeID(&host.PublicKey),
		StartHeight:            startHeight,
		EndHeight:              endHeight,
		TotalCost:              common.NewBigInt(funding.Int64()),
		ContractFee:            common.NewBigInt(host.ContractPrice.Int64()),
		LatestContractRevision: storageContractRevision,
		Status: storage.ContractStatus{
			UploadAbility: true,
			RenewAbility:  true,
		},
	}

	oldRoots, errRoots := oldContract.MerkleRoots()
	if errRoots != nil {
		return storage.ContractMetaData{}, errRoots
	}

	// store this contract info to client local
	contractMetaData, errInsert := cm.GetStorageContractSet().InsertContract(header, oldRoots)
	if errInsert != nil {
		return storage.ContractMetaData{}, errInsert
	}
	return contractMetaData, nil
}

// calculate Enode.ID, reference:
// p2p/discover/node.go:41
// p2p/discover/node.go:59
func PubkeyToEnodeID(pubkey *ecdsa.PublicKey) enode.ID {
	var pubBytes [64]byte
	math.ReadBits(pubkey.X, pubBytes[:len(pubBytes)/2])
	math.ReadBits(pubkey.Y, pubBytes[len(pubBytes)/2:])
	return enode.ID(crypto.Keccak256Hash(pubBytes[:]))
}

// calculate client and host collateral
func ClientPayoutsPreTax(host proto.StorageHostEntry, funding, basePrice, baseCollateral *big.Int, period, expectedStorage uint64) (clientPayout, hostPayout, hostCollateral *big.Int, err error) {

	// Divide by zero check.
	if host.StoragePrice.Sign() == 0 {
		host.StoragePrice.SetInt64(1)
	}

	// Underflow check.
	if funding.Cmp(host.ContractPrice) <= 0 {
		err = errors.New("underflow detected, funding < contractPrice")
		return
	}

	// Calculate clientPayout.
	clientPayout = new(big.Int).Sub(funding, host.ContractPrice)
	clientPayout = clientPayout.Sub(clientPayout, basePrice)

	// Calculate hostCollateral
	maxStorageSizeTime := new(big.Int).Div(clientPayout, host.StoragePrice)
	maxStorageSizeTime = maxStorageSizeTime.Mul(maxStorageSizeTime, host.Collateral)
	hostCollateral = maxStorageSizeTime.Add(maxStorageSizeTime, baseCollateral)
	host.Collateral = host.Collateral.Mul(host.Collateral, new(big.Int).SetUint64(period))
	host.Collateral = host.Collateral.Mul(host.Collateral, new(big.Int).SetUint64(expectedStorage))
	maxClientCollateral := host.Collateral.Mul(host.Collateral, new(big.Int).SetUint64(5))
	if hostCollateral.Cmp(maxClientCollateral) > 0 {
		hostCollateral = maxClientCollateral
	}

	// Don't add more collateral than the host is willing to put into a single
	// contract.
	if hostCollateral.Cmp(host.MaxCollateral) > 0 {
		hostCollateral = host.MaxCollateral
	}

	// Calculate hostPayout.
	hostCollateral.Add(hostCollateral, host.ContractPrice)
	hostPayout = hostCollateral.Add(hostCollateral, basePrice)
	return
}

//func (cm *ContractManager) managedRenew(contract *contractset.Contract, contractFunding *big.Int, newEndHeight uint64, allowance proto.Allowance, entry proto.StorageHostEntry, hostEnodeUrl string, clientPublic ecdsa.PublicKey) (storage.ContractMetaData, error) {
//
//	//Check if the storage contract ID meets the renew condition
//	status, ok := cm.managedContractStatus(contract.Header().ID)
//	if !ok || !status.RenewAbility {
//		return storage.ContractMetaData{}, errors.New("Condition not satisfied")
//	}
//
//	cm.lock.RLock()
//	//Calculate the required parameters
//	//TODO ClientPublicKey、HostEnodeUrl、Host、Allowance ?
//	params := proto.ContractParams{
//		Allowance:       allowance,
//		Host:            entry,
//		Funding:         contractFunding,
//		StartHeight:     cm.blockHeight,
//		EndHeight:       newEndHeight,
//		HostEnodeUrl:    hostEnodeUrl,
//		ClientPublicKey: clientPublic,
//	}
//	cm.lock.RUnlock()
//
//	newContract, errRenew := cm.ContractRenew(contract, params)
//	if errRenew != nil {
//		return storage.ContractMetaData{}, errRenew
//	}
//
//	return newContract, nil
//}
//
//func (cm *ContractManager) managedContractStatus(id storage.ContractID) (storage.ContractStatus, bool) {
//	//Concurrently secure access to contract status
//	mc, exists := cm.activeContracts.Acquire(id)
//	if !exists {
//		return storage.ContractStatus{}, false
//	}
//
//	return mc.Status(), true
//}
