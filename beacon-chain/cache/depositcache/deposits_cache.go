// Package depositcache is the source of validator deposits maintained
// in-memory by the beacon node – deposits processed from the
// eth1 powchain are then stored in this cache to be accessed by
// any other service during a beacon node's runtime.
package depositcache

import (
	"bytes"
	"context"
	"encoding/hex"
	"math/big"
	"sort"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	ethpb "github.com/prysmaticlabs/ethereumapis/eth/v1alpha1"
	dbpb "github.com/prysmaticlabs/prysm/proto/beacon/db"
	"github.com/prysmaticlabs/prysm/shared/bytesutil"
	"github.com/prysmaticlabs/prysm/shared/params"
	"github.com/prysmaticlabs/prysm/shared/trieutil"
	"github.com/sirupsen/logrus"
	"go.opencensus.io/trace"
)

var (
	historicalDepositsCount = promauto.NewCounter(prometheus.CounterOpts{
		Name: "beacondb_all_deposits",
		Help: "The number of total deposits in the beaconDB in-memory database",
	})
)

// DepositFetcher defines a struct which can retrieve deposit information from a store.
type DepositFetcher interface {
	AllDeposits(ctx context.Context, untilBlk *big.Int) []*ethpb.Deposit
	DepositByPubkey(ctx context.Context, pubKey []byte) (*ethpb.Deposit, *big.Int)
	DepositsNumberAndRootAtHeight(ctx context.Context, blockHeight *big.Int) (uint64, [32]byte)
	FinalizedDeposits(ctx context.Context) *FinalizedDeposits
	NonFinalizedDeposits(ctx context.Context, untilBlk *big.Int) []*ethpb.Deposit
}

// FinalizedDeposits stores the trie of deposits that have been included
// in the beacon state up to the latest finalized checkpoint.
type FinalizedDeposits struct {
	Deposits        *trieutil.SparseMerkleTrie
	MerkleTrieIndex int64
}

// DepositCache stores all in-memory deposit objects. This
// stores all the deposit related data that is required by the beacon-node.
type DepositCache struct {
	// Beacon chain deposits in memory.
	pendingDeposits   []*dbpb.DepositContainer
	deposits          []*dbpb.DepositContainer
	finalizedDeposits *FinalizedDeposits
	depositsLock      sync.RWMutex
}

// New instantiates a new deposit cache
func New() (*DepositCache, error) {
	finalizedDepositsTrie, err := trieutil.NewTrie(params.BeaconConfig().DepositContractTreeDepth)
	if err != nil {
		return nil, err
	}

	// finalizedDeposits.MerkleTrieIndex is initialized to -1 because it represents the index of the last trie item.
	// Inserting the first item into the trie will set the value of the index to 0.
	return &DepositCache{
		pendingDeposits:   []*dbpb.DepositContainer{},
		deposits:          []*dbpb.DepositContainer{},
		finalizedDeposits: &FinalizedDeposits{Deposits: finalizedDepositsTrie, MerkleTrieIndex: -1},
	}, nil
}

// InsertDeposit into the database. If deposit or block number are nil
// then this method does nothing.
func (dc *DepositCache) InsertDeposit(ctx context.Context, d *ethpb.Deposit, blockNum uint64, index int64, depositRoot [32]byte) {
	ctx, span := trace.StartSpan(ctx, "DepositsCache.InsertDeposit")
	defer span.End()
	if d == nil {
		log.WithFields(logrus.Fields{
			"block":        blockNum,
			"deposit":      d,
			"index":        index,
			"deposit root": hex.EncodeToString(depositRoot[:]),
		}).Warn("Ignoring nil deposit insertion")
		return
	}
	dc.depositsLock.Lock()
	defer dc.depositsLock.Unlock()
	// Keep the slice sorted on insertion in order to avoid costly sorting on retrieval.
	heightIdx := sort.Search(len(dc.deposits), func(i int) bool { return dc.deposits[i].Index >= index })
	newDeposits := append(
		[]*dbpb.DepositContainer{{Deposit: d, Eth1BlockHeight: blockNum, DepositRoot: depositRoot[:], Index: index}},
		dc.deposits[heightIdx:]...)
	dc.deposits = append(dc.deposits[:heightIdx], newDeposits...)
	historicalDepositsCount.Inc()
}

// InsertDepositContainers inserts a set of deposit containers into our deposit cache.
func (dc *DepositCache) InsertDepositContainers(ctx context.Context, ctrs []*dbpb.DepositContainer) {
	ctx, span := trace.StartSpan(ctx, "DepositsCache.InsertDepositContainers")
	defer span.End()
	dc.depositsLock.Lock()
	defer dc.depositsLock.Unlock()

	sort.SliceStable(ctrs, func(i int, j int) bool { return ctrs[i].Index < ctrs[j].Index })
	dc.deposits = ctrs
	historicalDepositsCount.Add(float64(len(ctrs)))
}

// InsertFinalizedDeposits inserts deposits up to eth1DepositIndex (inclusive) into the finalized deposits cache.
func (dc *DepositCache) InsertFinalizedDeposits(ctx context.Context, eth1DepositIndex int64) {
	ctx, span := trace.StartSpan(ctx, "DepositsCache.InsertFinalizedDeposits")
	defer span.End()
	dc.depositsLock.Lock()
	defer dc.depositsLock.Unlock()

	depositTrie := dc.finalizedDeposits.Deposits
	insertIndex := int(dc.finalizedDeposits.MerkleTrieIndex + 1)
	for _, d := range dc.deposits {
		if d.Index <= dc.finalizedDeposits.MerkleTrieIndex {
			continue
		}
		if d.Index > eth1DepositIndex {
			break
		}
		depHash, err := d.Deposit.Data.HashTreeRoot()
		if err != nil {
			log.WithError(err).Error("Could not hash deposit data. Finalized deposit cache not updated.")
			return
		}
		depositTrie.Insert(depHash[:], insertIndex)
		insertIndex++
	}

	dc.finalizedDeposits = &FinalizedDeposits{
		Deposits:        depositTrie,
		MerkleTrieIndex: eth1DepositIndex,
	}
}

// AllDepositContainers returns all historical deposit containers.
func (dc *DepositCache) AllDepositContainers(ctx context.Context) []*dbpb.DepositContainer {
	ctx, span := trace.StartSpan(ctx, "DepositsCache.AllDepositContainers")
	defer span.End()
	dc.depositsLock.RLock()
	defer dc.depositsLock.RUnlock()

	return dc.deposits
}

// AllDeposits returns a list of historical deposits until the given block number
// (inclusive). If no block is specified then this method returns all historical deposits.
func (dc *DepositCache) AllDeposits(ctx context.Context, untilBlk *big.Int) []*ethpb.Deposit {
	ctx, span := trace.StartSpan(ctx, "DepositsCache.AllDeposits")
	defer span.End()
	dc.depositsLock.RLock()
	defer dc.depositsLock.RUnlock()

	var deposits []*ethpb.Deposit
	for _, ctnr := range dc.deposits {
		if untilBlk == nil || untilBlk.Uint64() >= ctnr.Eth1BlockHeight {
			deposits = append(deposits, ctnr.Deposit)
		}
	}
	return deposits
}

// DepositsNumberAndRootAtHeight returns the number of deposits that exist at a specified
// block height. If no deposits are found at that block height, we return the number of deposits
// at the first block height we find right beneath it. If nothing is found at all, we return 0 and
// the empty root.
func (dc *DepositCache) DepositsNumberAndRootAtHeight(ctx context.Context, blockHeight *big.Int) (uint64, [32]byte) {
	dc.depositsLock.RLock()
	defer dc.depositsLock.RUnlock()
	for i := len(dc.deposits) - 1; i >= 0; i-- {
		if dc.deposits[i].Eth1BlockHeight <= blockHeight.Uint64() {
			return uint64(dc.deposits[i].Index) + 1, bytesutil.ToBytes32(dc.deposits[i].DepositRoot)
		}
	}
	return 0, params.BeaconConfig().ZeroHash
}

// DepositByPubkey looks through historical deposits and finds one which contains
// a certain public key within its deposit data.
func (dc *DepositCache) DepositByPubkey(ctx context.Context, pubKey []byte) (*ethpb.Deposit, *big.Int) {
	ctx, span := trace.StartSpan(ctx, "DepositsCache.DepositByPubkey")
	defer span.End()
	dc.depositsLock.RLock()
	defer dc.depositsLock.RUnlock()

	var deposit *ethpb.Deposit
	var blockNum *big.Int
	for _, ctnr := range dc.deposits {
		if bytes.Equal(ctnr.Deposit.Data.PublicKey, pubKey) {
			deposit = ctnr.Deposit
			blockNum = big.NewInt(int64(ctnr.Eth1BlockHeight))
			break
		}
	}
	return deposit, blockNum
}

// FinalizedDeposits returns the finalized deposits trie.
func (dc *DepositCache) FinalizedDeposits(ctx context.Context) *FinalizedDeposits {
	ctx, span := trace.StartSpan(ctx, "DepositsCache.FinalizedDeposits")
	defer span.End()
	dc.depositsLock.RLock()
	defer dc.depositsLock.RUnlock()

	return &FinalizedDeposits{
		Deposits:        dc.finalizedDeposits.Deposits.Copy(),
		MerkleTrieIndex: dc.finalizedDeposits.MerkleTrieIndex,
	}
}

// NonFinalizedDeposits returns the list of non-finalized deposits until the given block number (inclusive).
// If no block is specified then this method returns all non-finalized deposits.
func (dc *DepositCache) NonFinalizedDeposits(ctx context.Context, untilBlk *big.Int) []*ethpb.Deposit {
	ctx, span := trace.StartSpan(ctx, "DepositsCache.NonFinalizedDeposits")
	defer span.End()
	dc.depositsLock.RLock()
	defer dc.depositsLock.RUnlock()

	if dc.finalizedDeposits == nil {
		return dc.AllDeposits(ctx, untilBlk)
	}

	lastFinalizedDepositIndex := dc.finalizedDeposits.MerkleTrieIndex
	var deposits []*ethpb.Deposit
	for _, d := range dc.deposits {
		if (d.Index > lastFinalizedDepositIndex) && (untilBlk == nil || untilBlk.Uint64() >= d.Eth1BlockHeight) {
			deposits = append(deposits, d.Deposit)
		}
	}

	return deposits
}

// PruneProofs removes proofs from all deposits whose index is equal or less than untilDepositIndex.
func (dc *DepositCache) PruneProofs(ctx context.Context, untilDepositIndex int64) error {
	ctx, span := trace.StartSpan(ctx, "DepositsCache.PruneProofs")
	defer span.End()
	dc.depositsLock.Lock()
	defer dc.depositsLock.Unlock()

	if untilDepositIndex >= int64(len(dc.deposits)) {
		untilDepositIndex = int64(len(dc.deposits) - 1)
	}

	for i := untilDepositIndex; i >= 0; i-- {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Finding a nil proof means that all proofs up to this deposit have been already pruned.
		if dc.deposits[i].Deposit.Proof == nil {
			break
		}
		dc.deposits[i].Deposit.Proof = nil
	}

	return nil
}
