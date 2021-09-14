package vochain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	lru "github.com/hashicorp/golang-lru"
	tmcrypto "github.com/tendermint/tendermint/crypto"
	ed25519 "github.com/tendermint/tendermint/crypto/ed25519"
	"github.com/vocdoni/arbo"
	"go.vocdoni.io/dvote/crypto/ethereum"
	"go.vocdoni.io/dvote/db/badgerdb"
	"go.vocdoni.io/dvote/log"
	"go.vocdoni.io/dvote/statedb"

	"go.vocdoni.io/dvote/types"
	models "go.vocdoni.io/proto/build/go/models"
	"google.golang.org/protobuf/proto"
)

// rootLeafGetRoot is the GetRootFn function for a leaf that is the root
// itself.
func rootLeafGetRoot(value []byte) ([]byte, error) {
	if len(value) != 32 {
		return nil, fmt.Errorf("len(value) = %v != 32", len(value))
	}
	return value, nil
}

// rootLeafSetRoot is the SetRootFn function for a leaf that is the root
// itself.
func rootLeafSetRoot(value []byte, root []byte) ([]byte, error) {
	if len(value) != 32 {
		return nil, fmt.Errorf("len(value) = %v != 32", len(value))
	}
	return root, nil
}

// processGetCensusRoot is the GetRootFn function to get the census root of a
// process leaf.
func processGetCensusRoot(value []byte) ([]byte, error) {
	var sdbProc models.StateDBProcess
	if err := proto.Unmarshal(value, &sdbProc); err != nil {
		return nil, fmt.Errorf("cannot unmarshal StateDBProcess: %w", err)
	}
	return sdbProc.Process.CensusRoot, nil
}

// processSetCensusRoot is the SetRootFn function to set the census root of a
// process leaf.
func processSetCensusRoot(value []byte, root []byte) ([]byte, error) {
	var sdbProc models.StateDBProcess
	if err := proto.Unmarshal(value, &sdbProc); err != nil {
		return nil, fmt.Errorf("cannot unmarshal StateDBProcess: %w", err)
	}
	sdbProc.Process.CensusRoot = root
	newValue, err := proto.Marshal(&sdbProc)
	if err != nil {
		return nil, fmt.Errorf("cannot marshal StateDBProcess: %w", err)
	}
	return newValue, nil
}

// processGetVotesRoot is the GetRootFn function to get the votes root of a
// process leaf.
func processGetVotesRoot(value []byte) ([]byte, error) {
	var sdbProc models.StateDBProcess
	if err := proto.Unmarshal(value, &sdbProc); err != nil {
		return nil, fmt.Errorf("cannot unmarshal StateDBProcess: %w", err)
	}
	return sdbProc.VotesRoot, nil
}

// processSetVotesRoot is the SetRootFn function to set the votes root of a
// process leaf.
func processSetVotesRoot(value []byte, root []byte) ([]byte, error) {
	var sdbProc models.StateDBProcess
	if err := proto.Unmarshal(value, &sdbProc); err != nil {
		return nil, fmt.Errorf("cannot unmarshal StateDBProcess: %w", err)
	}
	sdbProc.VotesRoot = root
	newValue, err := proto.Marshal(&sdbProc)
	if err != nil {
		return nil, fmt.Errorf("cannot marshal StateDBProcess: %w", err)
	}
	return newValue, nil
}

var (
	// OraclesCfg is the Oracles subTree configuration.
	OraclesCfg = statedb.NewTreeSingletonConfig(statedb.TreeParams{
		HashFunc:          arbo.HashFunctionSha256,
		KindID:            "oracs",
		MaxLevels:         256,
		ParentLeafGetRoot: rootLeafGetRoot,
		ParentLeafSetRoot: rootLeafSetRoot,
	})

	// ValidatorsCfg is the Validators subTree configuration.
	ValidatorsCfg = statedb.NewTreeSingletonConfig(statedb.TreeParams{
		HashFunc:          arbo.HashFunctionSha256,
		KindID:            "valids",
		MaxLevels:         256,
		ParentLeafGetRoot: rootLeafGetRoot,
		ParentLeafSetRoot: rootLeafSetRoot,
	})

	// ProcessesCfg is the Processes subTree configuration.
	ProcessesCfg = statedb.NewTreeSingletonConfig(statedb.TreeParams{
		HashFunc:          arbo.HashFunctionSha256,
		KindID:            "procs",
		MaxLevels:         256,
		ParentLeafGetRoot: rootLeafGetRoot,
		ParentLeafSetRoot: rootLeafSetRoot,
	})

	// CensusCfg is the Census subTree (found under a Process leaf) configuration
	// for a process that supports non-anonymous voting with rolling census.
	CensusCfg = statedb.NewTreeNonSingletonConfig(statedb.TreeParams{
		HashFunc:          arbo.HashFunctionSha256,
		KindID:            "cen",
		MaxLevels:         256,
		ParentLeafGetRoot: processGetCensusRoot,
		ParentLeafSetRoot: processSetCensusRoot,
	})

	// CensusPoseidonCfg is the Census subTree (found under a Process leaf)
	// configuration when the process supports anonymous voting with rolling
	// census.  This Census subTree uses the SNARK friendly hash function Poseidon.
	CensusPoseidonCfg = statedb.NewTreeNonSingletonConfig(statedb.TreeParams{
		HashFunc:          arbo.HashFunctionPoseidon,
		KindID:            "cenPos",
		MaxLevels:         64,
		ParentLeafGetRoot: processGetCensusRoot,
		ParentLeafSetRoot: processSetCensusRoot,
	})

	// VotesCfg is the Votes subTree (found under a Process leaf) configuration.
	VotesCfg = statedb.NewTreeNonSingletonConfig(statedb.TreeParams{
		HashFunc:          arbo.HashFunctionSha256,
		KindID:            "votes",
		MaxLevels:         256,
		ParentLeafGetRoot: processGetVotesRoot,
		ParentLeafSetRoot: processSetVotesRoot,
	})
)

// EventListener is an interface used for executing custom functions during the
// events of the block creation process.
// The order in which events are executed is: Rollback, OnVote, Onprocess, On..., Commit.
// The process is concurrency safe, meaning that there cannot be two sequences
// happening in parallel.
//
// If Commit() returns ErrHaltVochain, the error is considered a consensus
// failure and the blockchain will halt.
//
// If OncProcessResults() returns an error, the results transaction won't be included
// in the blockchain. This event relays on the event handlers to decide if results are
// valid or not since the Vochain State do not validate results.
type EventListener interface {
	OnVote(vote *models.Vote, txIndex int32)
	OnNewTx(blockHeight uint32, txIndex int32)
	OnProcess(pid, eid []byte, censusRoot, censusURI string, txIndex int32)
	OnProcessStatusChange(pid []byte, status models.ProcessStatus, txIndex int32)
	OnCancel(pid []byte, txIndex int32)
	OnProcessKeys(pid []byte, encryptionPub, commitment string, txIndex int32)
	OnRevealKeys(pid []byte, encryptionPriv, reveal string, txIndex int32)
	OnProcessResults(pid []byte, results *models.ProcessResult, txIndex int32) error
	// TODO: Add OnProcessStart(pids [][]byte)
	Commit(height uint32) (err error)
	Rollback()
}

type ErrHaltVochain struct {
	reason error
}

func (e ErrHaltVochain) Error() string { return fmt.Sprintf("halting vochain: %v", e.reason) }
func (e ErrHaltVochain) Unwrap() error { return e.reason }

// State represents the state of the vochain application
type State struct {
	Store             *statedb.StateDB
	Tx                *statedb.TreeTx
	mainTreeViewValue atomic.Value
	voteCache         *lru.Cache
	ImmutableState
	mempoolRemoveTxKeys func([][32]byte, bool)
	txCounter           int32
	eventListeners      []EventListener
	height              uint32
}

// NOTE(Edu): It is my understanding that all write operations to the State
// come from processing transaction, which are always processed serially, and
// thus no thread-safety is required.  The arbo-based StateDB supports
// concurrent reads to the TreeView while a new transaction is opened and
// written serially, so there should not be need for this mutex anymore.
// TODO: Remove the mutex and the locking once we're ready to do integration
// tests.
// ImmutableState holds the latest trees version saved on disk
type ImmutableState struct {
	// Note that the mutex locks the entirety of the three IAVL trees, both
	// their mutable and immutable components. An immutable tree is not safe
	// for concurrent use with its parent mutable tree.
	sync.RWMutex
}

// NewState creates a new State
func NewState(dataDir string) (*State, error) {
	var err error
	sdb, err := initStateDB(dataDir)
	if err != nil {
		return nil, fmt.Errorf("cannot init StateDB: %s", err)
	}
	voteCache, err := lru.New(voteCacheSize)
	if err != nil {
		return nil, err
	}
	version, err := sdb.Version()
	if err != nil {
		return nil, err
	}
	root, err := sdb.Hash()
	if err != nil {
		return nil, err
	}
	log.Infof("state database is ready at version %d with hash %x",
		version, root)
	tx, err := sdb.BeginTx()
	if err != nil {
		return nil, err
	}
	mainTreeView, err := sdb.TreeView(nil)
	if err != nil {
		return nil, err
	}
	s := &State{
		Store:     sdb,
		Tx:        tx,
		voteCache: voteCache,
	}
	s.setMainTreeView(mainTreeView)
	return s, nil
}

// initStateDB initializes the StateDB with the default subTrees
func initStateDB(dataDir string) (*statedb.StateDB, error) {
	log.Infof("initializing StateDB")
	db, err := badgerdb.New(badgerdb.Options{Path: dataDir})
	if err != nil {
		return nil, err
	}
	sdb := statedb.NewStateDB(db)
	startTime := time.Now()
	defer log.Infof("StateDB load took %s", time.Since(startTime))
	root, err := sdb.Hash()
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(root, make([]byte, len(root))) {
		// StateDB already initialized if StateDB.Root != emptyHash
		return sdb, nil
	}
	update, err := sdb.BeginTx()
	defer update.Discard()
	if err != nil {
		return nil, err
	}
	if err := update.Add(OraclesCfg.Key(),
		make([]byte, OraclesCfg.HashFunc().Len())); err != nil {
		return nil, err
	}
	if err := update.Add(ValidatorsCfg.Key(),
		make([]byte, ValidatorsCfg.HashFunc().Len())); err != nil {
		return nil, err
	}
	if err := update.Add(ProcessesCfg.Key(),
		make([]byte, ProcessesCfg.HashFunc().Len())); err != nil {
		return nil, err
	}
	header := models.TendermintHeader{}
	headerBytes, err := proto.Marshal(&header)
	if err != nil {
		return nil, err
	}
	if err := update.Add(headerKey, headerBytes); err != nil {
		return nil, err
	}
	return sdb, update.Commit()
}

// mainTreeView is a thread-safe function to obtain a pointer to the last
// opened mainTree as a TreeView.
func (v *State) mainTreeView() *statedb.TreeView {
	return v.mainTreeViewValue.Load().(*statedb.TreeView)
}

// setMainTreeView is a thread-safe function to store a pointer to the last
// opened mainTree as TreeView.
func (v *State) setMainTreeView(treeView *statedb.TreeView) {
	v.mainTreeViewValue.Store(treeView)
}

// mainTreeViewer returns the mainTree as a treeViewer.  When isQuery is false,
// the mainTree returned is the not yet commited one from the currently open
// StateDB transaction.  When isQuery is false, the mainTree returned is the
// last commited version.
func (v *State) mainTreeViewer(isQuery bool) statedb.TreeViewer {
	if isQuery {
		return v.mainTreeView()
	}
	return v.Tx.AsTreeView()
}

// AddEventListener adds a new event listener, to receive method calls on block
// events as documented in EventListener.
func (v *State) AddEventListener(l EventListener) {
	v.eventListeners = append(v.eventListeners, l)
}

var exist = []byte{1}

// AddOracle adds a trusted oracle given its address if not exists
func (v *State) AddOracle(address common.Address) error {
	v.Lock()
	defer v.Unlock()
	return v.Tx.DeepSet(address.Bytes(), exist, OraclesCfg)
}

// RemoveOracle removes a trusted oracle given its address if exists
func (v *State) RemoveOracle(address common.Address) error {
	v.Lock()
	defer v.Unlock()
	oracles, err := v.Tx.SubTree(OraclesCfg)
	if err != nil {
		return err
	}
	if _, err := oracles.Get(address.Bytes()); err == arbo.ErrKeyNotFound {
		return fmt.Errorf("oracle not found: %w", err)
	} else if err != nil {
		return err
	}
	return oracles.Set(address.Bytes(), nil)
}

// Oracles returns the current oracle list
func (v *State) Oracles(isQuery bool) ([]common.Address, error) {
	v.RLock()
	defer v.RUnlock()

	oraclesTree, err := v.mainTreeViewer(isQuery).SubTree(OraclesCfg)
	if err != nil {
		return nil, err
	}

	var oracles []common.Address
	if err := oraclesTree.Iterate(func(key, value []byte) bool {
		// removed oracles are still in the tree but with value set to nil
		if len(value) == 0 {
			return true
		}
		oracles = append(oracles, common.BytesToAddress(key))
		return true
	}); err != nil {
		return nil, err
	}
	return oracles, nil
}

// hexPubKeyToTendermintEd25519 decodes a pubKey string to a ed25519 pubKey
func hexPubKeyToTendermintEd25519(pubKey string) (tmcrypto.PubKey, error) {
	var tmkey ed25519.PubKey
	pubKeyBytes, err := hex.DecodeString(pubKey)
	if err != nil {
		return nil, err
	}
	if len(pubKeyBytes) != 32 {
		return nil, fmt.Errorf("pubKey length is invalid")
	}
	copy(tmkey[:], pubKeyBytes[:])
	return tmkey, nil
}

// AddValidator adds a tendemint validator if it is not already added
func (v *State) AddValidator(validator *models.Validator) error {
	v.Lock()
	defer v.Unlock()
	validatorBytes, err := proto.Marshal(validator)
	if err != nil {
		return err
	}
	return v.Tx.DeepSet(validator.Address, validatorBytes, ValidatorsCfg)
}

// RemoveValidator removes a tendermint validator identified by its address
func (v *State) RemoveValidator(address []byte) error {
	v.Lock()
	defer v.Unlock()
	validators, err := v.Tx.SubTree(ValidatorsCfg)
	if err != nil {
		return err
	}
	if _, err := validators.Get(address); err == arbo.ErrKeyNotFound {
		return fmt.Errorf("validator not found: %w", err)
	} else if err != nil {
		return err
	}
	return validators.Set(address, nil)
}

// Validators returns a list of the validators saved on persistent storage
func (v *State) Validators(isQuery bool) ([]*models.Validator, error) {
	v.RLock()
	defer v.RUnlock()

	validatorsTree, err := v.mainTreeViewer(isQuery).SubTree(ValidatorsCfg)
	if err != nil {
		return nil, err
	}

	var validators []*models.Validator
	var callbackErr error
	if err := validatorsTree.Iterate(func(key, value []byte) bool {
		// removed validators are still in the tree but with value set
		// to nil
		if len(value) == 0 {
			return true
		}
		validator := &models.Validator{}
		if err := proto.Unmarshal(value, validator); err != nil {
			callbackErr = err
			return false
		}
		validators = append(validators, validator)
		return true
	}); err != nil {
		return nil, err
	}
	if callbackErr != nil {
		return nil, callbackErr
	}
	return validators, nil
}

// AddProcessKeys adds the keys to the process
func (v *State) AddProcessKeys(tx *models.AdminTx) error {
	if tx.ProcessId == nil || tx.KeyIndex == nil {
		return fmt.Errorf("no processId or keyIndex provided on AddProcessKeys")
	}
	process, err := v.Process(tx.ProcessId, false)
	if err != nil {
		return err
	}
	if tx.CommitmentKey != nil {
		process.CommitmentKeys[*tx.KeyIndex] = fmt.Sprintf("%x", tx.CommitmentKey)
		log.Debugf("added commitment key %d for process %x: %x",
			*tx.KeyIndex, tx.ProcessId, tx.CommitmentKey)
	}
	if tx.EncryptionPublicKey != nil {
		process.EncryptionPublicKeys[*tx.KeyIndex] = fmt.Sprintf("%x", tx.EncryptionPublicKey)
		log.Debugf("added encryption key %d for process %x: %x",
			*tx.KeyIndex, tx.ProcessId, tx.EncryptionPublicKey)
	}
	if process.KeyIndex == nil {
		process.KeyIndex = new(uint32)
	}
	*process.KeyIndex++
	if err := v.updateProcess(process, tx.ProcessId); err != nil {
		return err
	}
	for _, l := range v.eventListeners {
		l.OnProcessKeys(tx.ProcessId, fmt.Sprintf("%x", tx.EncryptionPublicKey),
			fmt.Sprintf("%x", tx.CommitmentKey), v.TxCounter())
	}
	return nil
}

// RevealProcessKeys reveals the keys of a process
func (v *State) RevealProcessKeys(tx *models.AdminTx) error {
	if tx.ProcessId == nil || tx.KeyIndex == nil {
		return fmt.Errorf("no processId or keyIndex provided on AddProcessKeys")
	}
	process, err := v.Process(tx.ProcessId, false)
	if err != nil {
		return err
	}
	if process.KeyIndex == nil || *process.KeyIndex < 1 {
		return fmt.Errorf("no keys to reveal, keyIndex is < 1")
	}
	rkey := ""
	if tx.RevealKey != nil {
		rkey = fmt.Sprintf("%x", tx.RevealKey)
		process.RevealKeys[*tx.KeyIndex] = rkey // TBD: Change hex strings for []byte
		log.Debugf("revealed commitment key %d for process %x: %x",
			*tx.KeyIndex, tx.ProcessId, tx.RevealKey)
	}
	ekey := ""
	if tx.EncryptionPrivateKey != nil {
		ekey = fmt.Sprintf("%x", tx.EncryptionPrivateKey)
		process.EncryptionPrivateKeys[*tx.KeyIndex] = ekey
		log.Debugf("revealed encryption key %d for process %x: %x",
			*tx.KeyIndex, tx.ProcessId, tx.EncryptionPrivateKey)
	}
	*process.KeyIndex--
	if err := v.updateProcess(process, tx.ProcessId); err != nil {
		return err
	}
	for _, l := range v.eventListeners {
		l.OnRevealKeys(tx.ProcessId, ekey, rkey, v.TxCounter())
	}
	return nil
}

// AddVote adds a new vote to a process and call the even listeners to OnVote.
// This method does not check if the vote already exist!
func (v *State) AddVote(vote *models.Vote) error {
	vid, err := v.voteID(vote.ProcessId, vote.Nullifier)
	if err != nil {
		return err
	}
	// save block number
	vote.Height = v.Height()
	voteBytes, err := proto.Marshal(vote)
	if err != nil {
		return fmt.Errorf("cannot marshal vote: %w", err)
	}
	sdbVote := models.StateDBVote{
		VoteHash:  ethereum.HashRaw(voteBytes),
		ProcessId: vote.ProcessId,
		Nullifier: vote.Nullifier,
	}
	sdbVoteBytes, err := proto.Marshal(&sdbVote)
	if err != nil {
		return fmt.Errorf("cannot marshal sdbVote: %w", err)
	}
	v.Lock()
	err = v.Tx.DeepAdd(vid, sdbVoteBytes, ProcessesCfg, VotesCfg.WithKey(vote.ProcessId))
	v.Unlock()
	if err != nil {
		return err
	}
	for _, l := range v.eventListeners {
		l.OnVote(vote, v.TxCounter())
	}
	return nil
}

// NOTE(Edu): Changed this from byte(processID+nullifier) to
// hash(processID+nullifier) to allow using it as a key in Arbo tree.
// voteID = hash(processID+nullifier)
func (v *State) voteID(pid, nullifier []byte) ([]byte, error) {
	if len(pid) != types.ProcessIDsize {
		return nil, fmt.Errorf("wrong processID size %d", len(pid))
	}
	if len(nullifier) != types.VoteNullifierSize {
		return nil, fmt.Errorf("wrong nullifier size %d", len(nullifier))
	}
	vid := sha256.New()
	vid.Write(pid)
	vid.Write(nullifier)
	return vid.Sum(nil), nil
}

// Envelope returns the hash of a stored vote if exists.
func (v *State) Envelope(processID, nullifier []byte, isQuery bool) (_ []byte, err error) {
	vid, err := v.voteID(processID, nullifier)
	if err != nil {
		return nil, err
	}
	v.RLock()
	defer v.RUnlock() // needs to be deferred due to the recover above
	votesTree, err := v.mainTreeViewer(isQuery).DeepSubTree(
		ProcessesCfg, VotesCfg.WithKey(processID))
	if err == arbo.ErrKeyNotFound {
		return nil, ErrProcessNotFound
	} else if err != nil {
		return nil, err
	}
	sdbVoteBytes, err := votesTree.Get(vid)
	if err == arbo.ErrKeyNotFound {
		return nil, ErrVoteDoesNotExist
	} else if err != nil {
		return nil, err
	}
	var sdbVote models.StateDBVote
	if err := proto.Unmarshal(sdbVoteBytes, &sdbVote); err != nil {
		return nil, fmt.Errorf("cannot unmarshal sdbVote: %w", err)
	}
	return sdbVote.VoteHash, nil
}

// EnvelopeExists returns true if the envelope identified with voteID exists
func (v *State) EnvelopeExists(processID, nullifier []byte, isQuery bool) (bool, error) {
	e, err := v.Envelope(processID, nullifier, isQuery)
	if err != nil && err != ErrVoteDoesNotExist {
		return false, err
	}
	if err == ErrVoteDoesNotExist {
		return false, nil
	}
	return e != nil, nil
}

// iterateVotes iterates fn over state tree entries with the processID prefix.
// if isQuery, the IAVL tree is used, otherwise the AVL tree is used.
func (v *State) iterateVotes(processID []byte,
	fn func(vid []byte, sdbVote *models.StateDBVote) bool, isQuery bool) error {
	v.RLock()
	defer v.RUnlock()
	votesTree, err := v.mainTreeViewer(isQuery).DeepSubTree(
		ProcessesCfg, VotesCfg.WithKey(processID))
	if err != nil {
		return err
	}
	var callbackErr error
	if err := votesTree.Iterate(func(key, value []byte) bool {
		var sdbVote models.StateDBVote
		if err := proto.Unmarshal(value, &sdbVote); err != nil {
			callbackErr = err
			return true
		}
		return fn(key, &sdbVote)
	}); err != nil {
		return err
	}
	if callbackErr != nil {
		return callbackErr
	}
	return nil
}

// CountVotes returns the number of votes registered for a given process id
func (v *State) CountVotes(processID []byte, isQuery bool) uint32 {
	var count uint32
	// TODO: Once statedb.TreeView.Size() works, replace this by that.
	v.iterateVotes(processID, func(vid []byte, sdbVote *models.StateDBVote) bool {
		count++
		return false
	}, isQuery)
	return count
}

// EnvelopeList returns a list of registered envelopes nullifiers given a processId
func (v *State) EnvelopeList(processID []byte, from, listSize int,
	isQuery bool) (nullifiers [][]byte) {
	idx := 0
	v.iterateVotes(processID, func(vid []byte, sdbVote *models.StateDBVote) bool {
		if idx >= from+listSize {
			return true
		}
		if idx >= from {
			nullifiers = append(nullifiers, sdbVote.Nullifier)
		}
		idx++
		return false
	}, isQuery)
	return nullifiers
}

// TODO: Add a funciton called SetHeader that is called in app.go where `app.State.Tx.Set(headerKey, headerBytes)`
// Query height -> list of process ID that will start, and call listeners OnProcessStart.

// Header returns the blockchain last block committed height
func (v *State) Header(isQuery bool) *models.TendermintHeader {
	v.RLock()
	headerBytes, err := v.mainTreeViewer(isQuery).Get(headerKey)
	v.RUnlock()
	if err != nil {
		log.Fatalf("cannot get headerKey from mainTree: %s", err)
	}
	var header models.TendermintHeader
	if err := proto.Unmarshal(headerBytes, &header); err != nil {
		log.Fatalf("cannot get proto.Unmarshal header: %s", err)
	}
	return &header
}

// Save persistent save of vochain mem trees
func (v *State) Save() ([]byte, error) {
	v.Lock()
	err := func() error {
		if err := v.Tx.Commit(); err != nil {
			return fmt.Errorf("cannot commit statedb tx: %w", err)
		}
		var err error
		if v.Tx, err = v.Store.BeginTx(); err != nil {
			return fmt.Errorf("cannot begin statedb tx: %w", err)
		}
		return nil
	}()
	v.Unlock()
	if err != nil {
		return nil, err
	}
	mainTreeView, err := v.Store.TreeView(nil)
	if err != nil {
		return nil, fmt.Errorf("cannot get statdeb mainTreeView: %w", err)
	}
	v.setMainTreeView(mainTreeView)
	height := uint32(v.Header(false).Height)
	for _, l := range v.eventListeners {
		if err := l.Commit(height); err != nil {
			if _, fatal := err.(ErrHaltVochain); fatal {
				return nil, err
			}
			log.Warnf("event callback error on commit: %v", err)
		}
	}
	atomic.StoreUint32(&v.height, height)
	// TODO: Figure out a way to notify via event the fact that a process
	// startBlock == height.  For example, keep a persistent map of
	// startBlock -> []processID, and query it and call
	// l.ProcessStartBlock([]processID).  Or via the commit, if the
	// listener has a list of processes indexed by startBlock (in
	// persistent storage), that works too.
	return v.Store.Hash()
}

// Rollback rollbacks to the last persistent db data version
func (v *State) Rollback() {
	for _, l := range v.eventListeners {
		l.Rollback()
	}
	v.Lock()
	defer v.Unlock()
	v.Tx.Discard()
	var err error
	if v.Tx, err = v.Store.BeginTx(); err != nil {
		log.Fatalf("cannot begin statedb tx: %s", err)
	}
	atomic.StoreInt32(&v.txCounter, 0)
}

// Height returns the current state height (block count)
func (v *State) Height() uint32 {
	return atomic.LoadUint32(&v.height)
}

// WorkingHash returns the hash of the vochain StateDB (mainTree.Root)
func (v *State) WorkingHash() []byte {
	v.RLock()
	defer v.RUnlock()
	hash, err := v.Tx.Root()
	if err != nil {
		panic(fmt.Sprintf("cannot get statedb mainTree root: %s", err))
	}
	return hash
}

// TxCounterAdd adds to the atomic transaction counter
func (v *State) TxCounterAdd() {
	atomic.AddInt32(&v.txCounter, 1)
}

// TxCounter returns the current tx count
func (v *State) TxCounter() int32 {
	return atomic.LoadInt32(&v.txCounter)
}
