// Copyright 2019 DxChain, All rights reserved.
// Use of this source code is governed by an Apache
// License 2.0 that can be found in the LICENSE file

package contractmanager

import (
	"fmt"

	"github.com/DxChainNetwork/godx/accounts"
	"github.com/DxChainNetwork/godx/common"
	"github.com/DxChainNetwork/godx/core/types"
	"github.com/DxChainNetwork/godx/storage"
)

// ContractCreateNegotiate will try to create the contract with storage host. Client will draft a storage contract
// and negotiate with storage host
// 1. draft the storage contract
// 2. negotiate the drafted storage contract
// 3. negotiate the storage contract revision
// 4. send the storage contract create transaction, once the storage contract revision negotiation succeed
// 5. commit the contract information, send success message to storage host, and handle host's response
func (cm *ContractManager) ContractCreateNegotiate(params storage.ContractParams) (meta storage.ContractMetaData, negotiateErr error) {
	// extract needed variables from the contract parameters
	hostInfo, paymentAddress := params.Host, params.ClientPaymentAddress

	// form unlock condition
	uc := formUnlockCondition(paymentAddress, hostInfo.PaymentAddress)

	// 1. draft the storage contract
	storageContract, err := draftStorageContract(hostInfo, params.RentPayment, params.Funding, params.StartHeight, params.EndHeight, paymentAddress, uc)
	if err != nil {
		negotiateErr = err
		cm.log.Error("contract create negotiation failed: failed to draft the storage contract", "err", err.Error())
		return
	}

	// find the wallet based on the account address, the information is needed
	// to sign the storage contract and storage contract revision
	account := accounts.Account{Address: paymentAddress}
	wallet, err := cm.b.AccountManager().Find(account)
	if err != nil {
		negotiateErr = err
		cm.log.Error("contract create negotiation failed: failed to find the account address", "err", err.Error(), "address", paymentAddress)
		return
	}

	// set up the connection
	sp, err := cm.b.SetupConnection(hostInfo.EnodeURL)
	if err != nil {
		negotiateErr = err
		cm.log.Error("contract create negotiation failed: failed to set up the connection", "err", err.Error())
		return
	}

	// handleNegotiationErr will handle the errors occurred in the negotiation process
	defer cm.handleNegotiationErr(&negotiateErr, hostInfo.EnodeID, sp)

	// 2. draft storage contract negotiation
	if storageContract, err = draftStorageContractNegotiate(sp, account, wallet, storageContract, types.StorageContractRevision{}); err != nil {
		negotiateErr = err
		cm.log.Error("contract create negotiation failed: failed to negotiate the drafted storage contract", "err", err.Error())
		return
	}

	// 3. storage contract revision negotiate
	storageContractRevision, err := storageContractRevisionNegotiate(sp, storageContract, uc, account, wallet)
	if err != nil {
		negotiateErr = err
		cm.log.Error("contract create negotiation failed: failed to negotiate the storage contract revision", "err", err.Error())
		return
	}

	// 4. send the storage contract create transaction
	if err := sendStorageContractCreateTx(storageContract, paymentAddress, cm.b); err != nil {
		negotiateErr = err
		cm.log.Error("contract create negotiation failed: failed to send the storage contract create transaction", "err", err.Error())
		return
	}

	// 5. commit the contract information, send success message to host, and handle host's response
	meta, negotiateErr = cm.clientStorageContractCommit(sp, hostInfo.EnodeID, params.StartHeight, params.Funding, hostInfo.ContractPrice, storageContract.ID(), storageContractRevision, nil)
	return
}

// draftStorageContract will draft a storage contract based on the information provided
func draftStorageContract(hostInfo storage.HostInfo, rentPayment storage.RentPayment, funding common.BigInt, startHeight uint64, endHeight uint64, paymentAddress common.Address, uc types.UnlockConditions) (types.StorageContract, error) {
	// calculate the client and host payouts
	baseDeposit := common.BigInt0
	basePrice := common.BigInt0
	clientPayout, hostPayout, _, err := calculatePayoutsAndHostDeposit(hostInfo, funding, basePrice, baseDeposit, startHeight, endHeight, rentPayment)
	if err != nil {
		err = fmt.Errorf("failed to draft the storage contract: %s", err.Error())
		return types.StorageContract{}, err
	}

	// draft the storage contract
	storageContract := types.StorageContract{
		FileSize:         0,
		FileMerkleRoot:   common.Hash{}, // no proof possible without data
		WindowStart:      endHeight,
		WindowEnd:        endHeight + hostInfo.WindowSize,
		ClientCollateral: types.DxcoinCollateral{DxcoinCharge: types.DxcoinCharge{Value: clientPayout.BigIntPtr(), Address: paymentAddress}},
		HostCollateral:   types.DxcoinCollateral{DxcoinCharge: types.DxcoinCharge{Value: hostPayout.BigIntPtr(), Address: hostInfo.PaymentAddress}},
		UnlockHash:       uc.UnlockHash(),
		RevisionNumber:   0,
		ValidProofOutputs: []types.DxcoinCharge{
			// Deposit is returned to client
			{Value: clientPayout.BigIntPtr(), Address: paymentAddress},
			// Deposit is returned to host
			{Value: hostPayout.BigIntPtr(), Address: hostInfo.PaymentAddress},
		},
		MissedProofOutputs: []types.DxcoinCharge{
			{Value: clientPayout.BigIntPtr(), Address: paymentAddress},
			{Value: hostPayout.BigIntPtr(), Address: hostInfo.PaymentAddress},
		},
	}

	return storageContract, nil
}

// formUnlockCondition will create unlock condition for drafted storage contract and
// storage contract revision
func formUnlockCondition(clientPaymentAddress common.Address, hostPaymentAddress common.Address) types.UnlockConditions {
	uc := types.UnlockConditions{
		PaymentAddresses: []common.Address{
			clientPaymentAddress,
			hostPaymentAddress,
		},
		SignaturesRequired: 2,
	}
	return uc
}