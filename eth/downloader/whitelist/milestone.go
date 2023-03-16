package whitelist

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/flags"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
)

type milestone struct {
	finality[*rawdb.Milestone]

	LockedMilestoneNumber uint64              // Locked sprint number
	LockedMilestoneHash   common.Hash         //Hash for the locked endBlock
	Locked                bool                //
	LockedMilestoneIDs    map[string]struct{} //list of milestone ids

	FutureMilestoneList  map[uint64]common.Hash // Future Milestone list
	FutureMilestoneOrder []uint64               // Future Milestone Order
	MaxCapacity          int                    //Capacity of future Milestone list
}

type milestoneService interface {
	finalityService

	GetMilestoneIDsList() []string
	RemoveMilestoneID(milestoneId string)
	LockMutex(endBlockNum uint64) bool
	UnlockMutex(doLock bool, milestoneId string, endBlockHash common.Hash)
	UnlockSprint(endBlockNum uint64)
	ProcessFutureMilestone(num uint64, hash common.Hash)
}

var (
	//Metrics for collecting the whitelisted milestone number
	whitelistedMilestoneMeter = metrics.NewRegisteredGauge("chain/milestone/latest", nil)

	//Metrics for collecting the future milestone number
	FutureMilestoneMeter = metrics.NewRegisteredGauge("chain/milestone/future", nil)

	//Metrics for collecting the length of the MilestoneIds map
	MilestoneIdsLengthMeter = metrics.NewRegisteredGauge("chain/milestone/idslength", nil)

	//Metrics for collecting the number of valid chains received
	MilestoneChainMeter = metrics.NewRegisteredMeter("chain/milestone/isvalidchain", nil)

	//Metrics for collecting the number of valid peers received
	MilestonePeerMeter = metrics.NewRegisteredMeter("chain/milestone/isvalidpeer", nil)
)

// IsValidChain checks the validity of chain by comparing it against the local
// milestone entries. It returns 2 values (with an error). The first boolean value represents
// if the chain is valid or not. The second boolean value represents if we need to
// skip the 'total difficulty' check or not. Note that it will only be true for cases when we've
// received a correct future chain (i.e. ahead of our current block and has a future milestone).
func (m *milestone) IsValidChain(currentHeader *types.Header, chain []*types.Header) (bool, bool, error) {
	var skipTdCheck bool

	log.Info("[DEBUG] In IsValidChain", "current", currentHeader.Number.Uint64(), "chain start", chain[0].Number.Uint64(), "chain end", chain[len(chain)-1].Number.Uint64())

	// Checking for the milestone flag
	if !flags.Milestone {
		return true, skipTdCheck, nil
	}

	m.finality.RLock()
	defer m.finality.RUnlock()

	var isValid bool = false

	defer func() {
		if isValid {
			MilestoneChainMeter.Mark(int64(1))
		} else {
			MilestoneChainMeter.Mark(int64(-1))
		}
	}()

	res, err := m.finality.IsValidChain(currentHeader, chain)
	log.Info("[DEBUG] Called finality.IsValidChain", "response", res)

	if !res {
		isValid = false
		return false, skipTdCheck, err
	}

	if m.Locked && !m.IsReorgAllowed(chain, m.LockedMilestoneNumber, m.LockedMilestoneHash) {
		isValid = false
		log.Info("[DEBUG] Returning due to reorg not allowed")
		return false, skipTdCheck, nil
	}

	isCompatible, skipTdCheck := m.IsFutureMilestoneCompatible(chain)
	log.Info("[DEBUG] Checked future milestone compatibility", "isCompatible", isCompatible, "skipTdCheck", skipTdCheck)
	if !isCompatible {
		isValid = false
		return false, skipTdCheck, nil
	}

	isValid = true

	return true, skipTdCheck, nil
}

// IsValidPeer checks if the chain we're about to receive from a peer is valid or not
// in terms of reorgs. We won't reorg beyond the last bor finality submitted to mainchain.
func (m *milestone) IsValidPeer(fetchHeadersByNumber func(number uint64, amount int, skip int, reverse bool) ([]*types.Header, []common.Hash, error)) (bool, error) {
	if !flags.Milestone {
		return true, nil
	}

	res, err := m.finality.IsValidPeer(fetchHeadersByNumber)

	if res {
		MilestonePeerMeter.Mark(int64(1))
	} else {
		MilestonePeerMeter.Mark(int64(-1))
	}

	return res, err
}

func (m *milestone) Process(block uint64, hash common.Hash) {
	m.finality.Lock()
	defer m.finality.Unlock()

	m.finality.Process(block, hash)

	for i := 0; i < len(m.FutureMilestoneOrder); i++ {
		if m.FutureMilestoneOrder[i] <= block {
			m.dequeueFutureMilestone()
		} else {
			break
		}
	}

	whitelistedMilestoneMeter.Update(int64(block))

	m.UnlockSprint(block)
}

// This function will Lock the mutex at the time of voting
// fixme: get rid of it
func (m *milestone) LockMutex(endBlockNum uint64) bool {
	m.finality.Lock()

	if m.doExist && endBlockNum <= m.Number { //if endNum is less than whitelisted milestone, then we won't lock the sprint
		log.Debug("endBlockNumber is less than or equal to latesMilestoneNumber", "endBlock Number", endBlockNum, "LatestMilestone Number", m.Number)

		return false
	}

	if m.Locked && endBlockNum != m.LockedMilestoneNumber {
		if endBlockNum < m.LockedMilestoneNumber {
			log.Debug("endBlockNum is less than locked milestone number", "endBlock Number", endBlockNum, "Locked Milestone Number", m.LockedMilestoneNumber)
			return false
		}

		log.Debug("endBlockNum is more than locked milestone number", "endBlock Number", endBlockNum, "Locked Milestone Number", m.LockedMilestoneNumber)
		m.UnlockSprint(m.LockedMilestoneNumber)
		m.Locked = false
	}

	m.LockedMilestoneNumber = endBlockNum

	return true
}

// This function will unlock the mutex locked in LockMutex
// fixme: get rid of it
func (m *milestone) UnlockMutex(doLock bool, milestoneId string, endBlockHash common.Hash) {
	m.Locked = m.Locked || doLock

	if doLock {
		m.LockedMilestoneHash = endBlockHash
		m.LockedMilestoneIDs[milestoneId] = struct{}{}
	}

	err := rawdb.WriteLockField(m.db, m.Locked, m.LockedMilestoneNumber, m.LockedMilestoneHash, m.LockedMilestoneIDs)
	if err != nil {
		log.Error("Error in writing lock data of milestone to db", "err", err)
	}

	milestoneIDLength := int64(len(m.LockedMilestoneIDs))
	MilestoneIdsLengthMeter.Update(milestoneIDLength)

	m.finality.Unlock()
}

// This function will unlock the locked sprint
func (m *milestone) UnlockSprint(endBlockNum uint64) {
	if endBlockNum < m.LockedMilestoneNumber {
		return
	}

	m.Locked = false
	m.purgeMilestoneIDsList()

	err := rawdb.WriteLockField(m.db, m.Locked, m.LockedMilestoneNumber, m.LockedMilestoneHash, m.LockedMilestoneIDs)

	if err != nil {
		log.Error("Error in writing lock data of milestone to db", "err", err)
	}
}

// This function will remove the stored milestoneID
func (m *milestone) RemoveMilestoneID(milestoneId string) {
	m.finality.Lock()

	delete(m.LockedMilestoneIDs, milestoneId)

	if len(m.LockedMilestoneIDs) == 0 {
		m.Locked = false
	}

	err := rawdb.WriteLockField(m.db, m.Locked, m.LockedMilestoneNumber, m.LockedMilestoneHash, m.LockedMilestoneIDs)
	if err != nil {
		log.Error("Error in writing lock data of milestone to db", "err", err)
	}

	m.finality.Unlock()
}

// This will check whether the incoming chain matches the locked sprint hash
func (m *milestone) IsReorgAllowed(chain []*types.Header, lockedMilestoneNumber uint64, lockedMilestoneHash common.Hash) bool {
	if chain[len(chain)-1].Number.Uint64() <= lockedMilestoneNumber { //Can't reorg if the end block of incoming
		return false //chain is less than locked sprint number
	}

	for i := 0; i < len(chain); i++ {
		if chain[i].Number.Uint64() == lockedMilestoneNumber {
			return chain[i].Hash() == lockedMilestoneHash
		}
	}

	return true
}

// This will return the list of milestoneIDs stored.
func (m *milestone) GetMilestoneIDsList() []string {
	m.finality.RLock()
	defer m.finality.RUnlock()

	// fixme: use generics :)
	keys := make([]string, 0, len(m.LockedMilestoneIDs))
	for key := range m.LockedMilestoneIDs {
		keys = append(keys, key)
	}

	return keys
}

// This is remove the milestoneIDs stored in the list.
func (m *milestone) purgeMilestoneIDsList() {
	m.LockedMilestoneIDs = make(map[string]struct{})
}

// IsFutureMilestoneCompatible checks the incoming chain against the future milestone
// list. It returns 2 boolean values. The first one represents whether it's good to
// proceed to import this chain. The second one represents whether the difficulty
// check can be skipped or not.
func (m *milestone) IsFutureMilestoneCompatible(chain []*types.Header) (bool, bool) {
	// Tip of the received chain
	chainTipNumber := chain[len(chain)-1].Number.Uint64()

	// skip represents whether to skip the difficulty check or not
	var skip bool

	for i := len(m.FutureMilestoneOrder) - 1; i >= 0; i-- {
		// Finding out the highest future milestone number
		// which is less or equal to received chain tip
		if chainTipNumber >= m.FutureMilestoneOrder[i] {
			// Looking for the received chain 's particular block number(matching future milestone number)
			for j := len(chain) - 1; j >= 0; j-- {
				if chain[j].Number.Uint64() == m.FutureMilestoneOrder[i] {
					endBlockNum := m.FutureMilestoneOrder[i]
					endBlockHash := m.FutureMilestoneList[endBlockNum]

					// Checking the received chain matches with future milestone
					match := chain[j].Hash() == endBlockHash
					if match {
						skip = true
					}

					return match, skip
				}
			}
		}
	}

	return true, skip
}

func (m *milestone) ProcessFutureMilestone(num uint64, hash common.Hash) {
	if len(m.FutureMilestoneOrder) < m.MaxCapacity {
		m.enqueueFutureMilestone(num, hash)
	}
}

// EnqueueFutureMilestone add the future milestone to the list
func (m *milestone) enqueueFutureMilestone(key uint64, hash common.Hash) {
	if _, ok := m.FutureMilestoneList[key]; ok {
		log.Info("[DEBUG] Future milestone already exist", "endBlockNumber", key, "futureMilestoneHash", hash)
		return
	}

	log.Info("[DEBUG] Enqueing new future milestone", "endBlockNumber", key, "futureMilestoneHash", hash)

	m.FutureMilestoneList[key] = hash
	m.FutureMilestoneOrder = append(m.FutureMilestoneOrder, key)

	err := rawdb.WriteFutureMilestoneList(m.db, m.FutureMilestoneOrder, m.FutureMilestoneList)
	if err != nil {
		log.Error("Error in writing future milestone data to db", "err", err)
	}

	FutureMilestoneMeter.Update(int64(key))
}

// DequeueFutureMilestone remove the future milestone entry from the list.
func (m *milestone) dequeueFutureMilestone() {
	delete(m.FutureMilestoneList, m.FutureMilestoneOrder[0])
	m.FutureMilestoneOrder = m.FutureMilestoneOrder[1:]

	err := rawdb.WriteFutureMilestoneList(m.db, m.FutureMilestoneOrder, m.FutureMilestoneList)
	if err != nil {
		log.Error("Error in writing future milestone data to db", "err", err)
	}
}
