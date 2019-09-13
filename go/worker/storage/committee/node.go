package committee

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"

	"github.com/eapache/channels"

	bolt "github.com/etcd-io/bbolt"

	"github.com/oasislabs/ekiden/go/common"
	"github.com/oasislabs/ekiden/go/common/accessctl"
	"github.com/oasislabs/ekiden/go/common/cbor"
	"github.com/oasislabs/ekiden/go/common/crypto/hash"
	"github.com/oasislabs/ekiden/go/common/crypto/signature"
	"github.com/oasislabs/ekiden/go/common/grpc"
	"github.com/oasislabs/ekiden/go/common/logging"
	"github.com/oasislabs/ekiden/go/common/node"
	"github.com/oasislabs/ekiden/go/common/pubsub"
	"github.com/oasislabs/ekiden/go/common/workerpool"
	roothashApi "github.com/oasislabs/ekiden/go/roothash/api"
	"github.com/oasislabs/ekiden/go/roothash/api/block"
	storageApi "github.com/oasislabs/ekiden/go/storage/api"
	"github.com/oasislabs/ekiden/go/storage/client"
	urkelNode "github.com/oasislabs/ekiden/go/storage/mkvs/urkel/node"
	"github.com/oasislabs/ekiden/go/worker/common/committee"
	"github.com/oasislabs/ekiden/go/worker/common/p2p"
)

var (
	_ committee.NodeHooks = (*Node)(nil)

	// ErrNonLocalBackend is the error returned when the storage backend doesn't implement the LocalBackend interface.
	ErrNonLocalBackend = errors.New("storage: storage backend doesn't support local storage")
)

const (
	// RoundLatest is a magic value for the latest round.
	RoundLatest = math.MaxUint64

	defaultUndefinedRound = ^uint64(0)
)

// outstandingMask records which storage roots still need to be synced or need to be retried.
type outstandingMask uint

const (
	maskNone  = outstandingMask(0x0)
	maskIO    = outstandingMask(0x1)
	maskState = outstandingMask(0x2)
	maskAll   = maskIO | maskState
)

func (o outstandingMask) String() string {
	var represented []string
	if o&maskIO != 0 {
		represented = append(represented, "io")
	}
	if o&maskState != 0 {
		represented = append(represented, "state")
	}
	return fmt.Sprintf("outstanding_mask{%s}", strings.Join(represented, ", "))
}

type roundItem interface {
	GetRound() uint64
}

// outOfOrderRoundQueue is a Round()-based min priority queue.
type outOfOrderRoundQueue []roundItem

// Sorting interface.
func (q outOfOrderRoundQueue) Len() int           { return len(q) }
func (q outOfOrderRoundQueue) Less(i, j int) bool { return q[i].GetRound() < q[j].GetRound() }
func (q outOfOrderRoundQueue) Swap(i, j int)      { q[i], q[j] = q[j], q[i] }

// Push appends x as the last element in the heap's array.
func (q *outOfOrderRoundQueue) Push(x interface{}) {
	*q = append(*q, x.(roundItem))
}

// Pop removes and returns the last element in the heap's array.
func (q *outOfOrderRoundQueue) Pop() interface{} {
	old := *q
	n := len(old)
	x := old[n-1]
	*q = old[0 : n-1]
	return x
}

// fetchedDiff has all the context needed for a single GetDiff operation.
type fetchedDiff struct {
	fetchMask outstandingMask
	fetched   bool
	err       error
	round     uint64
	prevRoot  urkelNode.Root
	thisRoot  urkelNode.Root
	writeLog  storageApi.WriteLog
}

func (d *fetchedDiff) GetRound() uint64 {
	return d.round
}

// blockSummary is a short summary of a single block.Block.
type blockSummary struct {
	Namespace common.Namespace `codec:"namespace"`
	Round     uint64           `codec:"round"`
	IORoot    urkelNode.Root   `codec:"io_root"`
	StateRoot urkelNode.Root   `codec:"state_root"`
}

func (s *blockSummary) GetRound() uint64 {
	return s.Round
}

func summaryFromBlock(blk *block.Block) *blockSummary {
	return &blockSummary{
		Namespace: blk.Header.Namespace,
		Round:     blk.Header.Round,
		IORoot: urkelNode.Root{
			Namespace: blk.Header.Namespace,
			Round:     blk.Header.Round,
			Hash:      blk.Header.IORoot,
		},
		StateRoot: urkelNode.Root{
			Namespace: blk.Header.Namespace,
			Round:     blk.Header.Round,
			Hash:      blk.Header.StateRoot,
		},
	}
}

// watcherState is the (persistent) watcher state.
type watcherState struct {
	LastBlock blockSummary `codec:"last_block"`
}

// Node watches blocks for storage changes.
type Node struct {
	commonNode *committee.Node

	logger *logging.Logger

	localStorage   storageApi.LocalBackend
	storageClient  storageApi.ClientBackend
	grpcPolicy     *grpc.DynamicRuntimePolicyChecker
	undefinedRound uint64

	fetchPool *workerpool.Pool

	stateStore *bolt.DB
	bucketName []byte

	syncedLock  sync.RWMutex
	syncedState watcherState

	blockCh    *channels.InfiniteChannel
	diffCh     chan *fetchedDiff
	finalizeCh chan *blockSummary

	ctx       context.Context
	ctxCancel context.CancelFunc

	quitCh chan struct{}
	initCh chan struct{}
}

func NewNode(
	commonNode *committee.Node,
	grpcPolicy *grpc.DynamicRuntimePolicyChecker,
	fetchPool *workerpool.Pool,
	db *bolt.DB,
	bucket []byte,
) (*Node, error) {
	localStorage, ok := commonNode.Storage.(storageApi.LocalBackend)
	if !ok {
		return nil, ErrNonLocalBackend
	}

	node := &Node{
		commonNode: commonNode,

		logger: logging.GetLogger("worker/storage/committee").With("runtime_id", commonNode.RuntimeID),

		localStorage: localStorage,
		grpcPolicy:   grpcPolicy,

		fetchPool: fetchPool,

		stateStore: db,
		bucketName: bucket,

		blockCh:    channels.NewInfiniteChannel(),
		diffCh:     make(chan *fetchedDiff),
		finalizeCh: make(chan *blockSummary),

		quitCh: make(chan struct{}),
		initCh: make(chan struct{}),
	}

	node.syncedState.LastBlock.Round = defaultUndefinedRound
	err := db.View(func(tx *bolt.Tx) error {
		bkt := tx.Bucket(bucket)

		bytes := bkt.Get(commonNode.RuntimeID[:])
		if bytes != nil {
			return cbor.Unmarshal(bytes, &node.syncedState)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	node.ctx, node.ctxCancel = context.WithCancel(context.Background())

	// Create a new storage client that will be used for remote sync.
	scl, err := client.New(node.ctx, node.commonNode.Identity, node.commonNode.Scheduler, node.commonNode.Registry)
	if err != nil {
		return nil, err
	}
	node.storageClient = scl.(storageApi.ClientBackend)
	if err := node.storageClient.WatchRuntime(commonNode.RuntimeID); err != nil {
		node.logger.Error("error watching storage runtime",
			"err", err,
		)
		return nil, err
	}

	return node, nil
}

// Service interface.

// Name returns the service name.
func (n *Node) Name() string {
	return "committee node"
}

// Start causes the worker to start responding to tendermint new block events.
func (n *Node) Start() error {
	go n.worker()
	return nil
}

// Stop causes the worker to stop watching and shut down.
func (n *Node) Stop() {
	n.ctxCancel()
}

// Quit returns a channel that will be closed when the worker stops.
func (n *Node) Quit() <-chan struct{} {
	return n.quitCh
}

// Cleanup cleans up any leftover state after the worker is stopped.
func (n *Node) Cleanup() {
	// Nothing to do here?
}

// Initialized returns a channel that will be closed once the worker finished starting up.
func (n *Node) Initialized() <-chan struct{} {
	return n.initCh
}

// NodeHooks implementation.

func (n *Node) HandlePeerMessage(context.Context, *p2p.Message) (bool, error) {
	// Nothing to do here.
	return false, nil
}

// Guarded by CrossNode.
func (n *Node) HandleEpochTransitionLocked(snapshot *committee.EpochSnapshot) {
	// Create new storage gRPC access policy for the current runtime.
	policy := accessctl.NewPolicy()
	for _, cc := range snapshot.GetComputeCommittees() {
		if cc != nil {
			computeCommitteePolicy.AddRulesForCommittee(&policy, cc)
		}
	}
	if tsc := snapshot.GetTransactionSchedulerCommittee(); tsc != nil {
		txnSchedulerCommitteePolicy.AddRulesForCommittee(&policy, tsc)
	}
	if mc := snapshot.GetMergeCommittee(); mc != nil {
		mergeCommitteePolicy.AddRulesForCommittee(&policy, mc)
	}
	// TODO: Query registry only for storage nodes after
	// https://github.com/oasislabs/ekiden/issues/1923 is implemented.
	nodes, err := n.commonNode.Registry.GetNodes(context.Background())
	if nodes != nil {
		storageNodesPolicy.AddRulesForNodeRoles(&policy, nodes, node.RoleStorageWorker)
	} else {
		n.logger.Error("couldn't get nodes from registry", "err", err)
	}
	// Update storage gRPC access policy for the current runtime.
	n.grpcPolicy.SetAccessPolicy(policy, n.commonNode.RuntimeID)
	n.logger.Debug("set new storage gRPC access policy", "policy", policy)
}

// Guarded by CrossNode.
func (n *Node) HandleNewBlockEarlyLocked(*block.Block) {
	// Nothing to do here.
}

// Guarded by CrossNode.
func (n *Node) HandleNewBlockLocked(blk *block.Block) {
	select {
	case n.blockCh.In() <- blk:
	case <-n.ctx.Done():
	}
}

// Guarded by CrossNode.
func (n *Node) HandleNewEventLocked(*roothashApi.Event) {
	// Nothing to do here.
}

// Watcher implementation.

// GetLastSynced returns the height, IORoot hash and StateRoot hash of the last block that was fully synced to.
func (n *Node) GetLastSynced() (uint64, urkelNode.Root, urkelNode.Root) {
	n.syncedLock.RLock()
	defer n.syncedLock.RUnlock()

	return n.syncedState.LastBlock.Round, n.syncedState.LastBlock.IORoot, n.syncedState.LastBlock.StateRoot
}

// ForceFinalize forces a storage finalization for the given round.
func (n *Node) ForceFinalize(ctx context.Context, runtimeID signature.PublicKey, round uint64) error {
	n.logger.Debug("forcing round finalization",
		"round", round,
		"runtime_id", runtimeID,
	)

	var block *block.Block
	var err error

	if round == RoundLatest {
		block, err = n.commonNode.Roothash.GetLatestBlock(ctx, runtimeID)
	} else {
		block, err = n.commonNode.Roothash.GetBlock(ctx, runtimeID, round)
	}

	if err != nil {
		return err
	}
	return n.localStorage.Finalize(ctx, block.Header.Namespace, round, []hash.Hash{
		block.Header.IORoot,
		block.Header.StateRoot,
	})
}

func (n *Node) fetchDiff(round uint64, prevRoot *urkelNode.Root, thisRoot *urkelNode.Root, fetchMask outstandingMask) {
	result := &fetchedDiff{
		fetchMask: fetchMask,
		fetched:   false,
		round:     round,
		prevRoot:  *prevRoot,
		thisRoot:  *thisRoot,
	}
	defer func() {
		n.diffCh <- result
	}()
	// Check if the new root doesn't already exist.
	if !n.localStorage.HasRoot(*thisRoot) {
		result.fetched = true
		if thisRoot.Hash.Equal(&prevRoot.Hash) {
			// Even if HasRoot returns false the root can still exist if it is equal
			// to the previous root and the root was emitted by the consensus committee
			// directly (e.g., during an epoch transition). In this case we need to
			// still apply the (empty) write log.
			result.writeLog = storageApi.WriteLog{}
		} else {
			// New root does not yet exist in storage and we need to fetch it from a
			// remote node.
			n.logger.Debug("calling GetDiff",
				"old_root", prevRoot,
				"new_root", thisRoot,
				"fetch_mask", fetchMask,
			)

			it, err := n.storageClient.GetDiff(n.ctx, *prevRoot, *thisRoot)
			if err != nil {
				result.err = err
				return
			}
			for {
				more, err := it.Next()
				if err != nil {
					result.err = err
					return
				}
				if !more {
					break
				}

				chunk, err := it.Value()
				if err != nil {
					result.err = err
					return
				}
				result.writeLog = append(result.writeLog, chunk)
			}
		}
	}
}

func (n *Node) finalize(summary *blockSummary) {
	err := n.localStorage.Finalize(n.ctx, summary.Namespace, summary.Round, []hash.Hash{
		summary.IORoot.Hash,
		summary.StateRoot.Hash,
	})
	switch err {
	case nil:
		n.logger.Debug("storage round finalized",
			"round", summary.Round,
		)
	case storageApi.ErrAlreadyFinalized:
		// This can happen if we are restoring after a roothash migration or if
		// we crashed before updating the sync state.
		n.logger.Warn("storage round already finalized",
			"round", summary.Round,
		)
	default:
		n.logger.Error("failed to finalize storage round",
			"err", err,
			"round", summary.Round,
		)
	}

	n.finalizeCh <- summary
}

type inFlight struct {
	outstanding   outstandingMask
	awaitingRetry outstandingMask
}

func (n *Node) worker() { // nolint: gocyclo
	defer close(n.quitCh)
	defer close(n.diffCh)

	// Wait for the common node to be initialized.
	select {
	case <-n.commonNode.Initialized():
	case <-n.ctx.Done():
		close(n.initCh)
		return
	}

	n.logger.Info("starting committee node")

	genesisBlock, err := n.commonNode.Roothash.GetGenesisBlock(n.ctx, n.commonNode.RuntimeID)
	if err != nil {
		n.logger.Error("can't retrieve genesis block", "err", err)
		return
	}
	n.undefinedRound = genesisBlock.Header.Round - 1

	// Subscribe to pruned roothash blocks.
	var pruneCh <-chan *roothashApi.PrunedBlock
	var pruneSub *pubsub.Subscription
	pruneCh, pruneSub, err = n.commonNode.Roothash.WatchPrunedBlocks()
	if err != nil {
		n.logger.Error("failed to watch pruned blocks", "err", err)
		return
	}
	defer pruneSub.Close()

	var fetcherGroup sync.WaitGroup

	n.syncedLock.RLock()
	cachedLastRound := n.syncedState.LastBlock.Round
	n.syncedLock.RUnlock()
	if cachedLastRound == defaultUndefinedRound || cachedLastRound < genesisBlock.Header.Round {
		cachedLastRound = n.undefinedRound
	}

	n.logger.Info("worker initialized",
		"genesis_round", genesisBlock.Header.Round,
		"last_synced", cachedLastRound,
	)

	outOfOrderDiffs := &outOfOrderRoundQueue{}
	outOfOrderApplieds := &outOfOrderRoundQueue{}
	syncingRounds := make(map[uint64]*inFlight)
	hashCache := make(map[uint64]*blockSummary)
	lastFullyAppliedRound := cachedLastRound

	heap.Init(outOfOrderDiffs)

	close(n.initCh)

	// Main processing loop. When a new block comes in, its state and io roots are inspected and their
	// writelogs fetched from remote storage nodes in case we don't have them locally yet. Fetches are
	// asynchronous and, once complete, trigger local Apply operations. These are serialized
	// per round (all applies for a given round have to be complete before applying anyting for following
	// rounds) using the outOfOrderDiffs priority queue and outOfOrderApplieds. Once a round has all its write
	// logs applied, a Finalize for it is triggered, again serialized by round but otherwise asynchronous
	// (outOfOrderApplieds and cachedLastRound).
mainLoop:
	for {
		// Drain the Apply and Finalize queues first, before waiting for new events in the select
		// below. Applies are drained first, followed by finalizations (which are asynchronous
		// but serialized, i.e. only one Finalize can be in progress at a time).

		// Apply any writelogs that came in through fetchDiff, but only if they are for the round
		// after the last fully applied one (lastFullyAppliedRound).
		if len(*outOfOrderDiffs) > 0 && lastFullyAppliedRound+1 == (*outOfOrderDiffs)[0].GetRound() {
			lastDiff := heap.Pop(outOfOrderDiffs).(*fetchedDiff)
			// Apply the write log if one exists.
			if lastDiff.fetched {
				_, err = n.localStorage.Apply(n.ctx, lastDiff.thisRoot.Namespace,
					lastDiff.prevRoot.Round, lastDiff.prevRoot.Hash,
					lastDiff.thisRoot.Round, lastDiff.thisRoot.Hash,
					lastDiff.writeLog)
				if err != nil {
					n.logger.Error("can't apply write log",
						"err", err,
						"old_root", lastDiff.prevRoot,
						"new_root", lastDiff.thisRoot,
					)
				}
			}

			// Check if we have fully synced the given round. If we have, we can proceed
			// with the Finalize operation.
			syncing := syncingRounds[lastDiff.round]
			syncing.outstanding &= ^lastDiff.fetchMask
			if syncing.outstanding == maskNone && syncing.awaitingRetry == maskNone {
				n.logger.Debug("finished syncing round", "round", lastDiff.round)
				delete(syncingRounds, lastDiff.round)
				summary := hashCache[lastDiff.round]
				delete(hashCache, lastDiff.round-1)

				// Finalize storage for this round. This happens asynchronously
				// with respect to Apply operations for subsequent rounds.
				lastFullyAppliedRound = lastDiff.round
				heap.Push(outOfOrderApplieds, summary)
			}

			continue
		}

		// Check if any new rounds were fully applied and need to be finalized. Only finalize
		// if it's the round after the one that was finalized last (cachedLastRound).
		// The finalization happens asynchronously with respect to this worker loop and any
		// applies that happen for subsequent rounds (which can proceed while earlier rounds are
		// still finalizing).
		if len(*outOfOrderApplieds) > 0 && cachedLastRound+1 == (*outOfOrderApplieds)[0].GetRound() {
			lastSummary := heap.Pop(outOfOrderApplieds).(*blockSummary)
			fetcherGroup.Add(1)
			go func() {
				defer fetcherGroup.Done()
				n.finalize(lastSummary)
			}()
			continue
		}

		select {
		case prunedBlk := <-pruneCh:
			n.logger.Debug("pruning storage for round", "round", prunedBlk.Round)

			// Prune given block.
			var ns common.Namespace
			copy(ns[:], prunedBlk.RuntimeID[:])

			if _, err = n.localStorage.Prune(n.ctx, ns, prunedBlk.Round); err != nil {
				n.logger.Error("failed to prune block",
					"err", err,
				)
				continue mainLoop
			}

		case inBlk := <-n.blockCh.Out():
			blk := inBlk.(*block.Block)
			n.logger.Debug("incoming block",
				"round", blk.Header.Round,
				"last_synced", cachedLastRound,
			)

			if _, ok := hashCache[cachedLastRound]; !ok && cachedLastRound == n.undefinedRound {
				dummy := blockSummary{
					Namespace: blk.Header.Namespace,
					Round:     cachedLastRound + 1,
				}
				dummy.IORoot.Empty()
				dummy.IORoot.Round = cachedLastRound + 1
				dummy.StateRoot.Empty()
				dummy.StateRoot.Round = cachedLastRound + 1
				hashCache[cachedLastRound] = &dummy
			}
			// Determine if we need to fetch any old block summaries. In case the first
			// round is an undefined round, we need to start with the following round
			// since the undefined round may be unsigned -1 and in this case the loop
			// would not do any iterations.
			startSummaryRound := cachedLastRound
			if startSummaryRound == n.undefinedRound {
				startSummaryRound++
			}
			for i := startSummaryRound; i < blk.Header.Round; i++ {
				if _, ok := hashCache[i]; ok {
					continue
				}
				var oldBlock *block.Block
				oldBlock, err = n.commonNode.Roothash.GetBlock(n.ctx, n.commonNode.RuntimeID, i)
				if err != nil {
					n.logger.Error("can't get block for round",
						"err", err,
						"round", i,
						"current_round", blk.Header.Round,
					)
					panic("can't get block in storage worker")
				}
				hashCache[i] = summaryFromBlock(oldBlock)
			}
			if _, ok := hashCache[blk.Header.Round]; !ok {
				hashCache[blk.Header.Round] = summaryFromBlock(blk)
			}

			for i := cachedLastRound + 1; i <= blk.Header.Round; i++ {
				syncing, ok := syncingRounds[i]
				if ok && syncing.outstanding == maskAll {
					continue
				}

				if !ok {
					syncing = &inFlight{
						outstanding:   maskNone,
						awaitingRetry: maskAll,
					}
					syncingRounds[i] = syncing
				}
				n.logger.Debug("preparing round sync",
					"round", i,
					"outstanding_mask", syncing.outstanding,
					"awaiting_retry", syncing.awaitingRetry,
				)

				prev := hashCache[i-1] // Closures take refs, so they need new variables here.
				this := hashCache[i]
				prevIORoot := urkelNode.Root{ // IO roots aren't chained, so clear it (but leave cache intact).
					Namespace: this.IORoot.Namespace,
					Round:     this.IORoot.Round,
				}
				prevIORoot.Hash.Empty()

				if (syncing.outstanding&maskIO) == 0 && (syncing.awaitingRetry&maskIO) != 0 {
					syncing.outstanding |= maskIO
					syncing.awaitingRetry &= ^maskIO
					fetcherGroup.Add(1)
					n.fetchPool.Submit(func() {
						defer fetcherGroup.Done()
						n.fetchDiff(this.Round, &prevIORoot, &this.IORoot, maskIO)
					})
				}
				if (syncing.outstanding&maskState) == 0 && (syncing.awaitingRetry&maskState) != 0 {
					syncing.outstanding |= maskState
					syncing.awaitingRetry &= ^maskState
					fetcherGroup.Add(1)
					n.fetchPool.Submit(func() {
						defer fetcherGroup.Done()
						n.fetchDiff(this.Round, &prev.StateRoot, &this.StateRoot, maskState)
					})
				}
			}

		case item := <-n.diffCh:
			if item.err != nil {
				n.logger.Error("error calling getdiff",
					"err", item.err,
					"round", item.round,
					"old_root", item.prevRoot,
					"new_root", item.thisRoot,
					"fetch_mask", item.fetchMask,
				)
				syncingRounds[item.round].outstanding &= ^item.fetchMask
				syncingRounds[item.round].awaitingRetry |= item.fetchMask
			} else {
				heap.Push(outOfOrderDiffs, item)
			}

		case finalized := <-n.finalizeCh:
			// No further sync or out of order handling needed here, since
			// only one finalize at a time is triggered (for round cachedLastRound+1)
			n.syncedLock.Lock()
			n.syncedState.LastBlock.Round = finalized.Round
			n.syncedState.LastBlock.IORoot = finalized.IORoot
			n.syncedState.LastBlock.StateRoot = finalized.StateRoot
			err = n.stateStore.Update(func(tx *bolt.Tx) error {
				bkt := tx.Bucket(n.bucketName)
				bytes := cbor.Marshal(&n.syncedState)
				return bkt.Put(n.commonNode.RuntimeID[:], bytes)
			})
			n.syncedLock.Unlock()
			cachedLastRound = finalized.Round
			if err != nil {
				n.logger.Error("can't store watcher state to database", "err", err)
			}

		case <-n.ctx.Done():
			break mainLoop
		}
	}

	fetcherGroup.Wait()
	// blockCh will be garbage-collected without being closed. It can potentially still contain
	// some new blocks, but only as many as were already in-flight at the point when the main
	// context was canceled.
}
