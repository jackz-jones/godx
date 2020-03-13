// Copyright 2019 DxChain, All rights reserved.
// Use of this source code is governed by an Apache
// License 2.0 that can be found in the LICENSE file

package dpos

import (
	"math/big"
	"math/rand"
	"sync"

	"github.com/DxChainNetwork/godx/common"
)

// randomAddressSelector is the random selection algorithm for selecting multiple addresses
// The entries are passed in during initialization
type randomAddressSelector interface {
	RandomSelect() []common.Address
}

const (
	typeLuckyWheel = iota
	typeLuckySpinner
	typeInvalidRandomSelector
)

// randomSelectAddress randomly select entries based on weight from the entries.
func randomSelectAddress(typeCode int, data randomSelectorEntries, seed int64, target int) ([]common.Address, error) {
	ras, err := newRandomAddressSelector(typeCode, data, seed, target)
	if err != nil {
		return []common.Address{}, err
	}
	return ras.RandomSelect(), nil
}

// newRandomAddressSelector creates a randomAddressSelector with sepecified typeCode
func newRandomAddressSelector(typeCode int, entries randomSelectorEntries, seed int64, target int) (randomAddressSelector, error) {
	switch typeCode {
	case typeLuckyWheel:
		return newLuckyWheel(entries, seed, target)
	case typeLuckySpinner:
		return newLuckySpinner(entries, seed, target)
	}
	return nil, errUnknownRandomAddressSelectorType
}

type luckySpinner struct {
	rand   *rand.Rand
	data   randomSelectorEntries
	cnt    int
	result []common.Address

	sumVotes   common.BigInt
	selectOnce sync.Once
}

// newLuckySpinner creates a new lucky spinner for random selection.
func newLuckySpinner(entries randomSelectorEntries, seed int64, cnt int) (*luckySpinner, error) {
	sumVotes := common.BigInt0
	for _, entry := range entries {
		sumVotes = sumVotes.Add(entry.vote)
	}
	ls := &luckySpinner{
		rand:     rand.New(rand.NewSource(seed)),
		cnt:      cnt,
		data:     make(randomSelectorEntries, len(entries)),
		result:   make([]common.Address, cnt),
		sumVotes: sumVotes,
	}
	// input entries are copied so that the input is not modified
	copy(ls.data, entries)
	return ls, nil
}

// RandomSelect randoms select cnt number of target addresses based on their vote.
// If calling RandomSelect multiple times, the result will be always the same since
// actual selection will only call exactly once.
func (ls *luckySpinner) RandomSelect() []common.Address {
	ls.selectOnce.Do(ls.randomSelect)
	return ls.result
}

// randomSelect randomly select cnt number of addresses based on vote.
func (ls *luckySpinner) randomSelect() {
	// If there is not enough data for select, shuffle and return them all.
	if len(ls.data) < ls.cnt {
		ls.shuffleAndWriteEntriesToResult()
		return
	}
	// Else execute the random selection
	for i := 0; i < ls.cnt; i++ {
		selectedIndex := ls.selectEntry()
		selectedEntry := ls.data[selectedIndex]
		ls.result[i] = selectedEntry.addr
		// Remove the entry from ls.data
		if selectedIndex == len(ls.data)-1 {
			ls.data = ls.data[:len(ls.data)-1]
		} else {
			ls.data = append(ls.data[:selectedIndex], ls.data[selectedIndex+1:]...)
		}
		// Subtract the vote weight from sumVotes
		ls.sumVotes = ls.sumVotes.Sub(selectedEntry.vote)
	}
}

// selectEntry select a single entry from the luckySpinner.
func (ls *luckySpinner) selectEntry() int {
	pick := randomBigInt(ls.rand, ls.sumVotes)
	for i, entry := range ls.data {
		vote := entry.vote
		if pick.Cmp(vote) < 0 {
			return i
		}
		pick = pick.Sub(vote)
	}
	// This shall never happen
	return len(ls.data) - 1
}

// shuffleAndWriteEntriesToResult shuffle and write the entries to results.
// The function is only used when the target is smaller than entry length.
func (ls *luckySpinner) shuffleAndWriteEntriesToResult() {
	list := ls.data.listAddresses()
	ls.rand.Shuffle(len(list), func(i, j int) {
		list[i], list[j] = list[j], list[i]
	})
	ls.result = list
	return
}

// luckyWheel is the structure for lucky wheel random selection
type luckyWheel struct {
	// Initialized fields
	rand    *rand.Rand
	entries randomSelectorEntries
	target  int

	// results
	results []common.Address

	// In process
	sumVotes common.BigInt
	once     sync.Once
}

// newLuckyWheel create a lucky wheel for random selection. target is used for specifying
// the target number to be selected
func newLuckyWheel(entries randomSelectorEntries, seed int64, target int) (*luckyWheel, error) {
	sumVotes := common.BigInt0
	for _, entry := range entries {
		sumVotes = sumVotes.Add(entry.vote)
	}
	lw := &luckyWheel{
		rand:     rand.New(rand.NewSource(seed)),
		target:   target,
		entries:  make(randomSelectorEntries, len(entries)),
		results:  make([]common.Address, target),
		sumVotes: sumVotes,
	}
	// Make a copy of the input entries. Thus the modification of lw.entries will not effect
	// the input entries
	copy(lw.entries, entries)
	return lw, nil
}

// RandomSelect return the result of the random selection of lucky wheel
func (lw *luckyWheel) RandomSelect() []common.Address {
	lw.once.Do(lw.randomSelect)
	return lw.results
}

// random is a helper function that randomly select addresses from the lucky wheel.
// The execution result is added to lw.results field
func (lw *luckyWheel) randomSelect() {
	// If the number of entries is less than target, return shuffled entries
	if len(lw.entries) < lw.target {
		lw.shuffleAndWriteEntriesToResult()
		return
	}
	// Else execute the random selection algorithm
	for i := 0; i < lw.target; i++ {
		// Execute the selection
		selectedIndex := lw.selectSingleEntry()
		selectedEntry := lw.entries[selectedIndex]
		// Add to result, and remove from entry
		lw.results[i] = selectedEntry.addr
		if selectedIndex == len(lw.entries)-1 {
			lw.entries = lw.entries[:len(lw.entries)-1]
		} else {
			lw.entries = append(lw.entries[:selectedIndex], lw.entries[selectedIndex+1:]...)
		}
		// Subtract the vote weight from sumVotes
		lw.sumVotes.Sub(selectedEntry.vote)
	}
}

// selectSingleEntry select a single entry from the lucky Wheel. Return the selected index.
// No values updated in this function.
func (lw *luckyWheel) selectSingleEntry() int {
	selected := randomBigInt(lw.rand, lw.sumVotes)
	for i, entry := range lw.entries {
		vote := entry.vote
		// The entry is selected
		if selected.Cmp(vote) < 0 {
			return i
		}
		selected = selected.Sub(vote)
	}
	// Sanity: This shall never reached if code is correct. If this happens, currently
	// return the last entry of the entries
	// TODO: Should we panic here?
	return len(lw.entries) - 1
}

// shuffleAndWriteEntriesToResult shuffle and write the entries to results.
// The function is only used when the target is smaller than entry length.
func (lw *luckyWheel) shuffleAndWriteEntriesToResult() {
	list := lw.entries.listAddresses()
	lw.rand.Shuffle(len(list), func(i, j int) {
		list[i], list[j] = list[j], list[i]
	})
	lw.results = list
	return
}

type (
	// randomSelectorEntries is the list of randomSelectorEntry
	randomSelectorEntries []*randomSelectorEntry

	// randomSelectorEntry is the entry in the lucky wheel
	randomSelectorEntry struct {
		addr common.Address
		vote common.BigInt
	}
)

// listAddresses return the list of addresses of the entries
func (entries randomSelectorEntries) listAddresses() []common.Address {
	res := make([]common.Address, 0, len(entries))
	for _, entry := range entries {
		res = append(res, entry.addr)
	}
	return res
}

// randomBigInt return a random big integer between 0 and max using r as randomization
func randomBigInt(r *rand.Rand, max common.BigInt) common.BigInt {
	randNum := new(big.Int).Rand(r, max.BigIntPtr())
	return common.PtrBigInt(randNum)
}

func (entries randomSelectorEntries) Swap(i, j int) { entries[i], entries[j] = entries[j], entries[i] }
func (entries randomSelectorEntries) Len() int      { return len(entries) }
func (entries randomSelectorEntries) Less(i, j int) bool {
	if entries[i].vote.Cmp(entries[j].vote) != 0 {
		return entries[i].vote.Cmp(entries[j].vote) == 1
	}
	return entries[i].addr.String() > entries[j].addr.String()
}
