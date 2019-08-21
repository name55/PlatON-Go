package core

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"

	"github.com/PlatONnetwork/PlatON-Go/crypto/sha3"
	"github.com/PlatONnetwork/PlatON-Go/params"
	"github.com/PlatONnetwork/PlatON-Go/x/gov"
	"github.com/PlatONnetwork/PlatON-Go/x/plugin"

	"github.com/PlatONnetwork/PlatON-Go/log"

	"github.com/PlatONnetwork/PlatON-Go/common"
	"github.com/PlatONnetwork/PlatON-Go/common/vm"
	"github.com/PlatONnetwork/PlatON-Go/core/snapshotdb"
	"github.com/PlatONnetwork/PlatON-Go/core/state"
	"github.com/PlatONnetwork/PlatON-Go/rlp"
	"github.com/PlatONnetwork/PlatON-Go/x/restricting"
	"github.com/PlatONnetwork/PlatON-Go/x/staking"
	"github.com/PlatONnetwork/PlatON-Go/x/xcom"
	"github.com/PlatONnetwork/PlatON-Go/x/xutil"
)

func genesisStakingData(snapdb snapshotdb.DB, g *Genesis, stateDB *state.StateDB, programVersion uint32) error {

	isDone := false
	switch {
	case nil == g.Config:
		isDone = true
	case nil == g.Config.Cbft:
		isDone = true
	case len(g.Config.Cbft.InitialNodes) == 0:
		isDone = true
	}

	if isDone {
		log.Warn("Genesis StakingData, the genesis config or cbft or initialNodes is nil, Not building staking data")
		return nil
	}

	if g.Config.Cbft.ValidatorMode != common.PPOS_VALIDATOR_MODE {
		log.Info("Init staking snapshotdb data, validatorMode is not ppos")
		return nil
	}

	version := xutil.CalcVersion(programVersion)

	var length int

	if int(xcom.ConsValidatorNum()) <= len(g.Config.Cbft.InitialNodes) {
		length = int(xcom.ConsValidatorNum())
	} else {
		length = len(g.Config.Cbft.InitialNodes)
	}
	initQueue := g.Config.Cbft.InitialNodes

	//b, _ := json.Marshal(initQueue)
	//log.Debug("genesisStakingData InitialNodes", "queue", string(b))

	validatorQueue := make(staking.ValidatorQueue, length)

	lastHash := common.ZeroHash

	for index := 0; index < length; index++ {

		node := initQueue[index]

		can := &staking.Candidate{
			NodeId:             node.Node.ID,
			BlsPubKey:          node.BlsPubKey,
			StakingAddress:     vm.PlatONFoundationAddress,
			BenefitAddress:     vm.RewardManagerPoolAddr,
			StakingTxIndex:     uint32(index + 1),
			ProgramVersion:     version,
			Status:             staking.Valided,
			StakingEpoch:       uint32(0),
			StakingBlockNum:    uint64(0),
			Shares:             xcom.StakeThreshold(),
			Released:           xcom.StakeThreshold(),
			ReleasedHes:        common.Big0,
			RestrictingPlan:    common.Big0,
			RestrictingPlanHes: common.Big0,
			Description: staking.Description{
				ExternalId: "",
				NodeName:   "platon.node." + fmt.Sprint(index+1),
				Website:    "www.platon.network",
				Details:    "The PlatON Node",
			},
		}

		nodeAddr, err := xutil.NodeId2Addr(can.NodeId)
		if err != nil {
			return fmt.Errorf("Failed to convert nodeID to address. nodeId:%s, error:%s", can.NodeId.String(), err.Error())
		}

		key := staking.CandidateKeyByAddr(nodeAddr)

		if val, err := rlp.EncodeToBytes(can); nil != err {
			return fmt.Errorf("Failed to Store Candidate Info: rlp encodeing failed. nodeId:%s, error:%s", can.NodeId.String(), err.Error())
		} else {
			if err := snapdb.PutBaseDB(key, val); nil != err {
				return fmt.Errorf("Failed to Store Candidate Info: PutBaseDB failed. nodeId:%s, error:%s", can.NodeId.String(), err.Error())
			}
			// generate hash by candidate info
			newHash := generateKVHash(key, val, lastHash)
			lastHash = newHash
			log.Debug("Call genesisStakingData: Store Candidate", "pposhash", lastHash.Hex())

		}

		powerKey := staking.TallyPowerKey(can.Shares, can.StakingBlockNum, can.StakingTxIndex, can.ProgramVersion)
		if err := snapdb.PutBaseDB(powerKey, nodeAddr.Bytes()); nil != err {
			return fmt.Errorf("Failed to Store Candidate Power: PutBaseDB failed. nodeId:%s, error:%s", can.NodeId.String(), err.Error())
		}
		// generate hash by candidate power
		newHash := generateKVHash(powerKey, nodeAddr.Bytes(), lastHash)
		lastHash = newHash
		log.Debug("Call genesisStakingData: Store Power", "pposhash", lastHash.Hex())

		// build validator queue for the first consensus epoch
		validator := &staking.Validator{
			NodeAddress:   nodeAddr,
			NodeId:        node.Node.ID,
			BlsPubKey:     node.BlsPubKey,
			StakingWeight: [staking.SWeightItem]string{fmt.Sprint(version), xcom.StakeThreshold().String(), "0", fmt.Sprint(index + 1)},
			ValidatorTerm: 0,
		}
		validatorQueue[index] = validator

	}

	// store the account staking Reference Count
	err := snapdb.PutBaseDB(staking.GetAccountStakeRcKey(vm.PlatONFoundationAddress), common.Uint64ToBytes(uint64(length)))
	if nil != err {
		return fmt.Errorf("Failed to Store Staking Account Reference Count. account: %s, error:%s", vm.PlatONFoundationAddress.Hex(), err.Error())
	}
	// generate hash by stake account reference count
	newHash := generateKVHash(staking.GetAccountStakeRcKey(vm.PlatONFoundationAddress), common.Uint64ToBytes(uint64(length)), lastHash)
	lastHash = newHash
	log.Debug("Call genesisStakingData: Store Ac", "pposhash", lastHash.Hex())

	validatorArr, err := rlp.EncodeToBytes(validatorQueue)
	if nil != err {
		return fmt.Errorf("Failed to rlp encodeing genesis validators. error:%s", err.Error())
	}

	/**
	Epoch
	*/
	// build epoch validators indexInfo
	verifierIndex := &staking.ValArrIndex{
		Start: 1,
		End:   xutil.CalcBlocksEachEpoch(),
	}
	epochIndexArr := make(staking.ValArrIndexQueue, 0)
	epochIndexArr = append(epochIndexArr, verifierIndex)

	// current epoch start and end indexs
	epoch_index, err := rlp.EncodeToBytes(epochIndexArr)
	if nil != err {
		return fmt.Errorf("Failed to Store Epoch Validators start and end index: rlp encodeing failed. error:%s", err.Error())
	}

	b, _ := json.Marshal(epochIndexArr)
	log.Debug("Before Store Epoch Index", "queue", string(b), "epoch calc", xutil.CalcBlocksEachEpoch())
	//
	//
	log.Debug("Call genesisStakingData: Before Store Epoch Index", "key", hex.EncodeToString(staking.GetEpochIndexKey()), "val", hex.EncodeToString(epoch_index), "pposhash", lastHash.Hex())
	//
	//
	//

	if err := snapdb.PutBaseDB(staking.GetEpochIndexKey(), epoch_index); nil != err {
		return fmt.Errorf("Failed to Store Epoch Validators start and end index: PutBaseDB failed. error:%s", err.Error())
	}

	// generate hash by epoch indexs
	newHash = generateKVHash(staking.GetEpochIndexKey(), epoch_index, lastHash)
	lastHash = newHash
	log.Debug("Call genesisStakingData: After Store Epoch index", "pposhash", lastHash.Hex())

	// Epoch validators
	if err := snapdb.PutBaseDB(staking.GetEpochValArrKey(verifierIndex.Start, verifierIndex.End), validatorArr); nil != err {
		return fmt.Errorf("Failed to Store Epoch Validators: PutBaseDB failed. error:%s", err.Error())
	}
	// generate hash by epoch validators
	newHash = generateKVHash(staking.GetEpochValArrKey(verifierIndex.Start, verifierIndex.End), validatorArr, lastHash)
	lastHash = newHash
	log.Debug("Call genesisStakingData: Store Epoch", "pposhash", lastHash.Hex())

	/**
	Round
	*/
	// build previous round validators indexInfo
	pre_indexInfo := &staking.ValArrIndex{
		Start: 0,
		End:   0,
	}
	// build current round validators indexInfo
	curr_indexInfo := &staking.ValArrIndex{
		Start: 1,
		End:   xutil.ConsensusSize(),
	}
	roundIndexArr := make(staking.ValArrIndexQueue, 0)
	roundIndexArr = append(roundIndexArr, pre_indexInfo)
	roundIndexArr = append(roundIndexArr, curr_indexInfo)

	// round index
	round_index, err := rlp.EncodeToBytes(roundIndexArr)
	if nil != err {
		return fmt.Errorf("Failed to Store Round Validators start and end indexs: rlp encodeing failed. error:%s", err.Error())
	}
	if err := snapdb.PutBaseDB(staking.GetRoundIndexKey(), round_index); nil != err {
		return fmt.Errorf("Failed to Store Round Validators start and end indexs: PutBaseDB failed. error:%s", err.Error())
	}
	// generate hash by round indexs
	newHash = generateKVHash(staking.GetRoundIndexKey(), round_index, lastHash)
	lastHash = newHash
	log.Debug("Call genesisStakingData: Store ROund index", "pposhash", lastHash.Hex())

	// Previous Round validator
	if err := snapdb.PutBaseDB(staking.GetRoundValArrKey(pre_indexInfo.Start, pre_indexInfo.End), validatorArr); nil != err {
		return fmt.Errorf("Failed to Store Previous Round Validators: PutBaseDB failed. error:%s", err.Error())
	}
	// generate hash by pre-round validators
	newHash = generateKVHash(staking.GetRoundValArrKey(pre_indexInfo.Start, pre_indexInfo.End), validatorArr, lastHash)
	lastHash = newHash
	log.Debug("Call genesisStakingData: Store pre Round", "pposhash", lastHash.Hex())

	// Current Round validator
	if err := snapdb.PutBaseDB(staking.GetRoundValArrKey(curr_indexInfo.Start, curr_indexInfo.End), validatorArr); nil != err {
		return fmt.Errorf("Failed to Store Current Round Validators: PutBaseDB failed. error:%s", err.Error())
	}
	// generate hash by curr-round validators
	newHash = generateKVHash(staking.GetRoundValArrKey(curr_indexInfo.Start, curr_indexInfo.End), validatorArr, lastHash)
	lastHash = newHash
	log.Debug("Call genesisStakingData: Store curr round", "pposhash", lastHash.Hex())

	log.Info("Call genesisStakingData, Store genesis pposHash by stake data", "pposHash", lastHash.Hex())

	stateDB.SetState(vm.StakingContractAddr, staking.GetPPOSHASHKey(), lastHash.Bytes())
	return nil
}

// genesisAllowancePlan writes the data of precompiled restricting contract, which used for the second year allowance
// and the third year allowance, to stateDB
func genesisAllowancePlan(stateDb *state.StateDB, issue *big.Int) error {
	account := vm.RewardManagerPoolAddr
	var (
		zeroEpoch  = new(big.Int).Mul(big.NewInt(622157424869165), big.NewInt(1E11))
		oneEpoch   = new(big.Int).Mul(big.NewInt(559657424869165), big.NewInt(1E11))
		twoEpoch   = new(big.Int).Mul(big.NewInt(495594924869165), big.NewInt(1E11))
		threeEpoch = new(big.Int).Mul(big.NewInt(429930862369165), big.NewInt(1E11))
		fourEpoch  = new(big.Int).Mul(big.NewInt(362625198306666), big.NewInt(1E11))
		fiveEpoch  = new(big.Int).Mul(big.NewInt(293636892642603), big.NewInt(1E11))
		sixEpoch   = new(big.Int).Mul(big.NewInt(222923879336939), big.NewInt(1E11))
		sevenEpoch = new(big.Int).Mul(big.NewInt(150443040698633), big.NewInt(1E11))
		eightEpoch = new(big.Int).Mul(big.NewInt(761501810943699), big.NewInt(1E10))
	)
	stateDb.AddBalance(account, zeroEpoch)
	needRelease := []*big.Int{oneEpoch, twoEpoch, threeEpoch, fourEpoch, fiveEpoch, sixEpoch, sevenEpoch, eightEpoch}

	restrictingPlans := make([]restricting.RestrictingPlan, 0)
	OneYearEpochs := xutil.EpochsPerYear()

	for key, value := range needRelease {
		epochs := OneYearEpochs * (uint64(key) + 1)
		restrictingPlans = append(restrictingPlans, restricting.RestrictingPlan{epochs, value})
	}
	plugin.CreateRestrictingRecord(account, stateDb, restrictingPlans)
	return nil
}

func genesisPluginState(g *Genesis, statedb *state.StateDB, genesisReward, genesisIssue *big.Int, programVersion uint32) error {

	isDone := false
	switch {
	case nil == g.Config:
		isDone = true
	case nil == g.Config.Cbft:
		isDone = true
	}

	if isDone {
		log.Warn("Genesis xxPlugin statedb, the genesis config or cbft is nil, Not Store plugin genesis state")
		return nil
	}

	if g.Config.Cbft.ValidatorMode != common.PPOS_VALIDATOR_MODE {
		log.Info("Init xxPlugin genesis statedb, validatorMode is not ppos")
		return nil
	}

	// Store genesis yearEnd reward balance item
	plugin.SetYearEndBalance(statedb, 0, genesisReward)

	// Store genesis Issue for LAT
	plugin.SetYearEndCumulativeIssue(statedb, 0, genesisIssue)

	log.Info("Store version for gov into genesis statedb", "real version", fmt.Sprintf("%d.%d.%d",
		params.VersionMajor, params.VersionMinor, params.VersionPatch), "uint32 version", programVersion)

	// Store genesis governance data
	activeVersionList := []gov.ActiveVersionValue{
		{ActiveVersion: programVersion, ActiveBlock: 0},
	}
	activeVersionListBytes, _ := json.Marshal(activeVersionList)
	statedb.SetState(vm.GovContractAddr, gov.KeyActiveVersions(), activeVersionListBytes)
	// Store restricting plans for increase issue for second and third year
	if err := genesisAllowancePlan(statedb, genesisIssue); nil != err {
		return err
	}
	// Store genesis last Epoch
	log.Info("Set latest epoch", "blockNumber", g.Number, "epoch", 0)
	plugin.SetLatestEpoch(statedb, uint64(0))
	return nil
}

func generateKVHash(k, v []byte, oldHash common.Hash) common.Hash {
	var buf bytes.Buffer
	buf.Write(k)
	buf.Write(v)
	buf.Write(oldHash.Bytes())
	return rlpHash(buf.Bytes())
}

func rlpHash(x interface{}) (h common.Hash) {
	hw := sha3.NewKeccak256()
	rlp.Encode(hw, x)
	hw.Sum(h[:0])
	return h
}
