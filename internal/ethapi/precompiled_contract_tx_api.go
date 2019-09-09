// Copyright 2019 DxChain, All rights reserved.
// Use of this source code is governed by an Apache
// License 2.0 that can be found in the LICENSE file

package ethapi

import (
	"context"
	"errors"
	"math/big"
	"strconv"
	"strings"

	"github.com/DxChainNetwork/godx/accounts"
	"github.com/DxChainNetwork/godx/common"
	"github.com/DxChainNetwork/godx/common/hexutil"
	"github.com/DxChainNetwork/godx/common/unit"
	"github.com/DxChainNetwork/godx/consensus/dpos"
	"github.com/DxChainNetwork/godx/core/state"
	"github.com/DxChainNetwork/godx/core/types"
	"github.com/DxChainNetwork/godx/log"
	"github.com/DxChainNetwork/godx/rlp"
	"github.com/DxChainNetwork/godx/rpc"
)

var (
	// dpos related parameters

	// defines the minimum deposit of candidate
	minDeposit = big.NewInt(1e18)

	// defines the minimum balance of candidate
	candidateThreshold = big.NewInt(1e18)
)

const (

	// MaxVoteCount is the max number of voted candidates in a vot tx
	MaxVoteCount = 30

	// StorageContractTxGas defines the default gas for storage contract tx
	StorageContractTxGas = 90000

	// DposTxGas defines the default gas for dpos tx
	DposTxGas = 1000000
)

// PrivateStorageContractTxAPI exposes the storage contract tx methods for the RPC interface
type PrivateStorageContractTxAPI struct {
	b         Backend
	nonceLock *AddrLocker
}

// NewPrivateStorageContractTxAPI creates a private RPC service with methods specific for storage contract tx.
func NewPrivateStorageContractTxAPI(b Backend, nonceLock *AddrLocker) *PrivateStorageContractTxAPI {
	return &PrivateStorageContractTxAPI{b, nonceLock}
}

// SendHostAnnounceTX submit a host announce tx to txpool, only for outer request, need to open cmd and RPC API
func (psc *PrivateStorageContractTxAPI) SendHostAnnounceTX(from common.Address) (common.Hash, error) {
	hostEnodeURL := psc.b.GetHostEnodeURL()
	hostAnnouncement := types.HostAnnouncement{
		NetAddress: hostEnodeURL,
	}

	hash := hostAnnouncement.RLPHash()
	sign, err := psc.b.SignByNode(hash.Bytes())
	if err != nil {
		return common.Hash{}, err
	}
	hostAnnouncement.Signature = sign

	payload, err := rlp.EncodeToBytes(hostAnnouncement)
	if err != nil {
		return common.Hash{}, err
	}

	to := common.Address{}
	to.SetBytes([]byte{9})

	ctx := context.Background()

	// construct args
	args := NewPrecompiledContractTxArgs(from, to, payload, nil, StorageContractTxGas)
	txHash, err := sendPrecompiledContractTx(ctx, psc.b, psc.nonceLock, args)
	if err != nil {
		return common.Hash{}, err
	}
	return txHash, nil
}

// SendContractCreateTX submit a storage contract creation tx, generally triggered in ContractCreate, not for outer request
func (psc *PrivateStorageContractTxAPI) SendContractCreateTX(from common.Address, input []byte) (common.Hash, error) {
	to := common.Address{}
	to.SetBytes([]byte{10})
	ctx := context.Background()

	// construct args
	args := NewPrecompiledContractTxArgs(from, to, input, nil, StorageContractTxGas)
	txHash, err := sendPrecompiledContractTx(ctx, psc.b, psc.nonceLock, args)
	if err != nil {
		return common.Hash{}, err
	}
	return txHash, nil
}

// SendContractRevisionTX submit a storage contract revision tx, only triggered when host received consensus change, not for outer request
func (psc *PrivateStorageContractTxAPI) SendContractRevisionTX(from common.Address, input []byte) (common.Hash, error) {
	to := common.Address{}
	to.SetBytes([]byte{11})
	ctx := context.Background()

	// construct args
	args := NewPrecompiledContractTxArgs(from, to, input, nil, StorageContractTxGas)
	txHash, err := sendPrecompiledContractTx(ctx, psc.b, psc.nonceLock, args)
	if err != nil {
		return common.Hash{}, err
	}
	return txHash, nil
}

// SendStorageProofTX submit a storage proof tx, only triggered when host received consensus change, not for outer request
func (psc *PrivateStorageContractTxAPI) SendStorageProofTX(from common.Address, input []byte) (common.Hash, error) {
	to := common.Address{}
	to.SetBytes([]byte{12})
	ctx := context.Background()

	// construct args
	args := NewPrecompiledContractTxArgs(from, to, input, nil, StorageContractTxGas)
	txHash, err := sendPrecompiledContractTx(ctx, psc.b, psc.nonceLock, args)
	if err != nil {
		return common.Hash{}, err
	}
	return txHash, nil
}

// PublicDposTxAPI exposes the dpos tx methods for the RPC interface
type PublicDposTxAPI struct {
	b         Backend
	nonceLock *AddrLocker
}

// NewPublicDposTxAPI construct a PublicDposTxAPI object
func NewPublicDposTxAPI(b Backend, nonceLock *AddrLocker) *PublicDposTxAPI {
	return &PublicDposTxAPI{b, nonceLock}
}

// SendApplyCandidateTx submit a apply candidate tx.
// the parameter ratio is the award distribution ratio that candidate state.
func (dpos *PublicDposTxAPI) SendApplyCandidateTx(fields map[string]interface{}) (common.Hash, error) {
	to := common.Address{}
	to.SetBytes([]byte{13})
	ctx := context.Background()

	// parse precompile contract tx args
	args, err := ParsePrecompileContractTxArgs(to, DposTxGas, fields)
	if err != nil {
		return common.Hash{}, err
	}

	stateDB, _, err := dpos.b.StateAndHeaderByNumber(ctx, rpc.LatestBlockNumber)
	if err != nil {
		return common.Hash{}, err
	}

	// check dpos tx
	err = CheckDposOperationTx(stateDB, args)
	if err != nil {
		return common.Hash{}, err
	}

	txHash, err := sendPrecompiledContractTx(ctx, dpos.b, dpos.nonceLock, args)
	if err != nil {
		return common.Hash{}, err
	}
	return txHash, nil
}

// SendCancelCandidateTx submit a cancel candidate tx
func (dpos *PublicDposTxAPI) SendCancelCandidateTx(from common.Address) (common.Hash, error) {
	to := common.Address{}
	to.SetBytes([]byte{14})
	ctx := context.Background()

	// construct args
	args := NewPrecompiledContractTxArgs(from, to, nil, nil, DposTxGas)

	stateDB, _, err := dpos.b.StateAndHeaderByNumber(ctx, rpc.LatestBlockNumber)
	if err != nil {
		return common.Hash{}, err
	}

	// check dpos tx
	err = CheckDposOperationTx(stateDB, args)
	if err != nil {
		return common.Hash{}, err
	}

	txHash, err := sendPrecompiledContractTx(ctx, dpos.b, dpos.nonceLock, args)
	if err != nil {
		return common.Hash{}, err
	}
	return txHash, nil
}

// SendVoteTx submit a vote tx
func (dpos *PublicDposTxAPI) SendVoteTx(fields map[string]interface{}) (common.Hash, error) {
	to := common.Address{}
	to.SetBytes([]byte{15})
	ctx := context.Background()

	// parse precompile contract tx args
	args, err := ParsePrecompileContractTxArgs(to, DposTxGas, fields)
	if err != nil {
		return common.Hash{}, err
	}

	stateDB, _, err := dpos.b.StateAndHeaderByNumber(ctx, rpc.LatestBlockNumber)
	if err != nil {
		return common.Hash{}, err
	}

	// check dpos tx
	err = CheckDposOperationTx(stateDB, args)
	if err != nil {
		return common.Hash{}, err
	}

	txHash, err := sendPrecompiledContractTx(ctx, dpos.b, dpos.nonceLock, args)
	if err != nil {
		return common.Hash{}, err
	}
	return txHash, nil
}

// SendCancelVoteTx submit a cancel vote tx
func (dpos *PublicDposTxAPI) SendCancelVoteTx(from common.Address) (common.Hash, error) {
	to := common.Address{}
	to.SetBytes([]byte{16})
	ctx := context.Background()

	// construct args
	args := NewPrecompiledContractTxArgs(from, to, nil, nil, DposTxGas)

	stateDB, _, err := dpos.b.StateAndHeaderByNumber(ctx, rpc.LatestBlockNumber)
	if err != nil {
		return common.Hash{}, err
	}

	// check dpos tx
	err = CheckDposOperationTx(stateDB, args)
	if err != nil {
		return common.Hash{}, err
	}

	txHash, err := sendPrecompiledContractTx(ctx, dpos.b, dpos.nonceLock, args)
	if err != nil {
		return common.Hash{}, err
	}
	return txHash, nil
}

// sendPrecompiledContractTx send precompiled contract tx，mostly need from、to、value、input（rlp encoded）
//
// NOTE: this is general func, you can construct different args to send detailed tx, like host announce、form contract、contract revision、storage proof.
// Actually, it need to set different PrecompiledContractTxArgs, like from、to、value、input
func sendPrecompiledContractTx(ctx context.Context, b Backend, nonceLock *AddrLocker, args *PrecompiledContractTxArgs) (common.Hash, error) {

	// find the account of the address from
	account := accounts.Account{Address: args.From}
	wallet, err := b.AccountManager().Find(account)
	if err != nil {
		return common.Hash{}, err
	}

	nonceLock.LockAddr(args.From)
	defer nonceLock.UnlockAddr(args.From)

	// construct tx
	tx, err := args.NewPrecompiledContractTx(ctx, b)
	if err != nil {
		return common.Hash{}, err
	}

	// get chain ID
	var chainID *big.Int
	if config := b.ChainConfig(); config.IsEIP155(b.CurrentBlock().Number()) {
		chainID = config.ChainID
	}

	// sign the tx by using from's wallet
	signed, err := wallet.SignTx(account, tx, chainID)
	if err != nil {
		return common.Hash{}, err
	}

	// send signed tx to txpool
	if err := b.SendTx(ctx, signed); err != nil {
		return common.Hash{}, err
	}

	return signed.Hash(), nil
}

// PrecompiledContractTxArgs represents the arguments to submit a precompiled contract tx into the transaction pool.
type PrecompiledContractTxArgs struct {
	From     common.Address  `json:"from"`
	To       common.Address  `json:"to"`
	Gas      *hexutil.Uint64 `json:"gas"`
	Value    *hexutil.Big    `json:"value"`
	GasPrice *hexutil.Big    `json:"gasPrice"`
	Nonce    *hexutil.Uint64 `json:"nonce"`
	Input    *hexutil.Bytes  `json:"input"`
}

// NewPrecompiledContractTx construct precompiled contract tx with args
func (args *PrecompiledContractTxArgs) NewPrecompiledContractTx(ctx context.Context, b Backend) (*types.Transaction, error) {
	price, err := b.SuggestPrice(ctx)
	if err != nil {
		return nil, err
	}
	args.GasPrice = (*hexutil.Big)(price)

	nonce, err := b.GetPoolNonce(ctx, args.From)
	if err != nil {
		return nil, err
	}
	args.Nonce = (*hexutil.Uint64)(&nonce)

	if args.To == (common.Address{}) {
		return nil, errors.New(`precompile contract tx without to`)
	}

	return types.NewTransaction(uint64(*args.Nonce), args.To, (*big.Int)(args.Value), uint64(*args.Gas), (*big.Int)(args.GasPrice), *args.Input), nil
}

// NewPrecompiledContractTxArgs construct precompiled contract tx args
func NewPrecompiledContractTxArgs(from, to common.Address, input []byte, value *big.Int, gas uint64) *PrecompiledContractTxArgs {
	args := &PrecompiledContractTxArgs{
		From: from,
		To:   to,
	}

	if input != nil {
		args.Input = (*hexutil.Bytes)(&input)
	} else {
		args.Input = new(hexutil.Bytes)
	}

	args.Gas = new(hexutil.Uint64)
	*(*uint64)(args.Gas) = gas

	if value != nil {
		args.Value = (*hexutil.Big)(value)
	} else {
		args.Value = new(hexutil.Big)
	}

	return args
}

// CheckDposOperationTx checks the dpos transaction's filed
func CheckDposOperationTx(stateDB *state.StateDB, args *PrecompiledContractTxArgs) error {
	balance := stateDB.GetBalance(args.From)
	emptyHash := common.Hash{}
	switch args.To {

	// check ApplyCandidate tx
	case common.BytesToAddress([]byte{13}):

		// to be a candidate need minimum balance of candidateThreshold,
		// which can stop flooding of applying candidate
		if balance.Cmp(candidateThreshold) < 0 {
			return ErrBalanceNotEnoughCandidateThreshold
		}

		// maybe already become a delegator, so should checkout the allowed balance whether enough for this deposit
		voteDeposit := new(big.Int).SetInt64(0)
		voteDepositHash := stateDB.GetState(args.From, dpos.KeyVoteDeposit)
		if voteDepositHash != emptyHash {
			voteDeposit = voteDepositHash.Big()
		}

		allowedBal := new(big.Int).Sub(balance, voteDeposit)
		if args.Value.ToInt().Sign() <= 0 || args.Value.ToInt().Cmp(allowedBal) > 0 {
			return ErrDepositValueNotSuitable
		}

		// check the deposit value which must more than minDeposit
		if args.Value.ToInt().Cmp(minDeposit) < 0 {
			return ErrCandidateDepositTooLow
		}

		return nil

	// check CancelCandidate tx
	case common.BytesToAddress([]byte{14}):
		depositHash := stateDB.GetState(args.From, dpos.KeyCandidateDeposit)
		if depositHash == emptyHash {
			log.Error("has not become candidate yet,so can not submit cancel candidate tx", "address", args.From.String())
			return ErrNotCandidate
		}
		return nil

	// check Vote tx
	case common.BytesToAddress([]byte{15}):
		if args.Input == nil {
			return ErrEmptyInput
		}

		// maybe already become a candidate, so should checkout the allowed balance whether enough for this deposit
		candidateDeposit := new(big.Int).SetInt64(0)
		candidateDepositHash := stateDB.GetState(args.From, dpos.KeyCandidateDeposit)
		if candidateDepositHash != emptyHash {
			candidateDeposit = candidateDepositHash.Big()
		}

		allowedBal := new(big.Int).Sub(balance, candidateDeposit)
		if args.Value.ToInt().Sign() <= 0 || args.Value.ToInt().Cmp(allowedBal) > 0 {
			return ErrDepositValueNotSuitable
		}

		return nil

	// check CancelVote tx
	case common.BytesToAddress([]byte{16}):
		depositHash := stateDB.GetState(args.From, dpos.KeyVoteDeposit)
		if depositHash == emptyHash {
			log.Error("has not voted before,so can not submit cancel vote tx", "address", args.From.String())
			return ErrHasNotVote
		}
		return nil

	default:
		return ErrUnknownPrecompileContractAddress
	}
}

// ParsePrecompileContractTxArgs parse the input fields to PrecompiledContractTxArgs format
func ParsePrecompileContractTxArgs(to common.Address, gas uint64, fields map[string]interface{}) (args *PrecompiledContractTxArgs, err error) {
	var data []byte
	var from common.Address
	var deposit common.BigInt
	for key, value := range fields {
		switch key {
		case "ratio":
			str, ok := value.(string)
			if !ok {
				return nil, ErrRatioNotStringFormat
			}

			ratio, err := strconv.ParseUint(str, 10, 8)
			if err != nil {
				return nil, ErrParseStringToUint
			}

			if ratio > uint64(100) {
				return nil, ErrInvalidAwardDistributionRatio
			}

			// convert uint8 to []byte
			data = []byte{uint8(ratio)}

		case "from":
			str, ok := value.(string)
			if !ok {
				return nil, ErrFromNotStringFormat
			}

			from = common.HexToAddress(str)

		case "deposit":
			str, ok := value.(string)
			if !ok {
				return nil, ErrDepositNotStringFormat
			}

			// parse to big int,like "1000camel"、"10dx" and so on
			deposit, err = unit.ParseCurrency(str)
			if err != nil {
				return nil, ErrParseStringToBigInt
			}

		case "candidates":
			str, ok := value.(string)
			if !ok {
				return nil, ErrCandidatesNotStringFormat
			}

			hexAddrs := strings.Split(str, ",")

			// limit the max vote count to 30
			if len(hexAddrs) > MaxVoteCount {
				return nil, ErrBeyondMaxVoteSize
			}

			candidates := make([]common.Address, 0)
			for _, hexAddr := range hexAddrs {
				addr := common.HexToAddress(hexAddr)
				candidates = append(candidates, addr)
			}

			data, err = rlp.EncodeToBytes(candidates)
			if err != nil {
				return nil, ErrRLPEncodeCandidates
			}

		default:
			return nil, ErrUnknownParameter
		}
	}

	args = NewPrecompiledContractTxArgs(from, to, data, deposit.BigIntPtr(), gas)
	return
}