// Copyright 2019 DxChain, All rights reserved.
// Use of this source code is governed by an Apache
// License 2.0 that can be found in the LICENSE file

package dpos

import "github.com/DxChainNetwork/godx/common"

const (
	// Fixed number of extra-data prefix bytes reserved for signer vanity
	extraVanity = 32

	// Fixed number of extra-data suffix bytes reserved for signer seal
	extraSeal = 65

	// Number of recent block signatures to keep in memory
	inmemorySignatures = 4096

	// MaxValidatorSize indicates that the max number of validators in dpos consensus
	MaxValidatorSize = 21

	// SafeSize indicates that the least number of validators in dpos consensus
	SafeSize = MaxValidatorSize*2/3 + 1

	// ConsensusSize indicates that a confirmed block needs the least number of validators to approve
	ConsensusSize = MaxValidatorSize*2/3 + 1

	// RewardRatioDenominator is the max value of reward ratio
	RewardRatioDenominator uint64 = 100

	// ThawingEpochDuration defines that if user cancel candidates or vote, the deposit will be thawed after 2 epochs
	ThawingEpochDuration = 2

	// eligibleValidatorDenominator defines the denominator of the minimum expected block. If a validator
	// produces block less than expected by this denominator, it is considered as ineligible.
	eligibleValidatorDenominator = 2

	// BlockInterval indicates that a block will be produced every 10 seconds
	BlockInterval = int64(10)

	// EpochInterval indicates that a new epoch will be elected every a day
	EpochInterval = int64(86400)

	// MaxVoteCount is the maximum number of candidates that a vote transaction could
	// include
	MaxVoteCount = 30
)

var (
	// Block reward in camel for successfully mining a block
	frontierBlockReward = common.NewBigIntUint64(1e18).MultInt64(5)

	// Block reward in camel for successfully mining a block upward from Byzantium
	byzantiumBlockReward = common.NewBigIntUint64(1e18).MultInt64(3)

	// Block reward in camel for successfully mining a block upward from Constantinople
	constantinopleBlockReward = common.NewBigIntUint64(1e18).MultInt64(2)

	// minDeposit defines the minimum deposit of candidate
	minDeposit = common.NewBigIntUint64(1e18).MultInt64(10000)
)
