// blockfacts records each block's per-transaction state diff during the node's only execution of
// the block, the import, and serves it as the answer to debug_traceBlockByHash.
//
// The diff is geth's prestateTracer in diff mode without code, the exact payload the indexer
// requests from debug_traceBlockByHash. Serving it from here spares the node a second execution.
package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/tracers"
	"github.com/ethereum/go-ethereum/eth/tracers/logger"
	_ "github.com/ethereum/go-ethereum/eth/tracers/native" // registers prestateTracer
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/params"
)

// BlockFactsName is the live tracer name for --vmtrace.
const BlockFactsName = "clickBlockFacts"

const blockFactsCapacity = 1024

// blockFactsPrestateConfig must match BlockDiff::tracer_options in the indexer.
var blockFactsPrestateConfig = json.RawMessage(`{"diffMode":true,"disableCode":true}`)

var (
	blockFactsSealed     = metrics.NewRegisteredCounter("click/blockfacts/sealed", nil)
	blockFactsIncomplete = metrics.NewRegisteredCounter("click/blockfacts/incomplete", nil)
	blockFactsTxFailed   = metrics.NewRegisteredCounter("click/blockfacts/tx_trace_failed", nil)
	blockFactsServed     = metrics.NewRegisteredCounter("click/blockfacts/served", nil)
	blockFactsFallback   = metrics.NewRegisteredCounter("click/blockfacts/fallback", nil)
)

// activeBlockFacts is the process's one collector. The API reaches it here, because the live
// tracer directory hands the chain only the hooks.
var activeBlockFacts atomic.Pointer[blockFactsCollector]

func init() {
	tracers.LiveDirectory.Register(BlockFactsName, newBlockFactsCollector)
}

// BlockFactsTx is one transaction's diff, in debug_traceBlockByHash's per-transaction shape.
type BlockFactsTx struct {
	TxHash common.Hash     `json:"txHash"`
	Result json.RawMessage `json:"result"`
}

type blockFactsConfig struct {
	// Capacity is how many recent blocks stay servable.
	Capacity int `json:"capacity"`
}

// blockFactsCollector builds each block's facts as the block executes.
//
// The hooks run on the import goroutine, one block at a time, so tx state needs no lock. mu guards only the served blocks, which the RPC reads concurrently.
type blockFactsCollector struct {
	chainConfig *params.ChainConfig

	block   *types.Block    // block executing now, from OnBlockStart
	tx      *tracers.Tracer // prestate tracer of the transaction executing now
	txHash  common.Hash
	pending map[common.Hash]json.RawMessage // diffs of transactions that succeeded, by tx hash

	mu       sync.RWMutex
	blocks   map[common.Hash][]BlockFactsTx
	ring     []common.Hash // block hashes in insertion order; the oldest is evicted first
	next     int
	capacity int
}

func newBlockFactsCollector(raw json.RawMessage) (*tracing.Hooks, error) {
	cfg := blockFactsConfig{Capacity: blockFactsCapacity}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("blockfacts: parse config: %w", err)
	}
	if cfg.Capacity <= 0 {
		return nil, fmt.Errorf("blockfacts: capacity must be positive, got %d", cfg.Capacity)
	}
	c := &blockFactsCollector{
		pending:  make(map[common.Hash]json.RawMessage),
		blocks:   make(map[common.Hash][]BlockFactsTx, cfg.Capacity),
		ring:     make([]common.Hash, cfg.Capacity),
		capacity: cfg.Capacity,
	}
	if !activeBlockFacts.CompareAndSwap(nil, c) {
		return nil, errors.New("blockfacts: a collector already exists in this process")
	}
	return &tracing.Hooks{
		OnBlockchainInit: c.onBlockchainInit,
		OnTxStart:        c.onTxStart,
		OnTxEnd:          c.onTxEnd,
		OnOpcode:         c.onOpcode,
		OnBlockStart:     c.onBlockStart,
		OnBlockEnd:       c.onBlockEnd,
	}, nil
}

func (c *blockFactsCollector) onBlockchainInit(chainConfig *params.ChainConfig) {
	c.chainConfig = chainConfig
}

func (c *blockFactsCollector) onTxStart(env *tracing.VMContext, tx *types.Transaction, from common.Address) {
	c.txHash = tx.Hash()
	t, err := tracers.DefaultDirectory.New("prestateTracer", &tracers.Context{TxHash: c.txHash}, blockFactsPrestateConfig, c.chainConfig)
	if err != nil {
		log.Error("blockfacts: prestate tracer unavailable", "tx", c.txHash, "err", err)
		blockFactsTxFailed.Inc(1)
		c.tx = nil
		return
	}
	c.tx = t
	if t.OnTxStart != nil {
		t.OnTxStart(env, tx, from)
	}
}

func (c *blockFactsCollector) onTxEnd(receipt *types.Receipt, err error) {
	t := c.tx
	c.tx = nil
	if t == nil {
		return
	}
	if t.OnTxEnd != nil {
		t.OnTxEnd(receipt, err)
	}
	// An error means the block producer dropped the transaction, so it has no diff to keep.
	if err != nil {
		return
	}
	result, rerr := t.GetResult()
	if rerr != nil {
		log.Error("blockfacts: prestate result unavailable", "tx", c.txHash, "err", rerr)
		blockFactsTxFailed.Inc(1)
		return
	}
	c.pending[c.txHash] = result
}

func (c *blockFactsCollector) onOpcode(pc uint64, op byte, gas, cost uint64, scope tracing.OpContext, rData []byte, depth int, err error) {
	if t := c.tx; t != nil && t.OnOpcode != nil {
		t.OnOpcode(pc, op, gas, cost, scope, rData, depth, err)
	}
}

// onBlockStart drops any diffs left by a block that never ended, and remembers the block to seal.
func (c *blockFactsCollector) onBlockStart(ev tracing.BlockEvent) {
	clear(c.pending)
	c.block = ev.Block
}

// onBlockEnd seals the block that just executed. A block that failed is not stored.
//
// It runs after the block is written but before the chain head event, so the facts are served by
// the time newHeads announces the block.
func (c *blockFactsCollector) onBlockEnd(err error) {
	block := c.block
	c.block = nil
	if err != nil || block == nil {
		clear(c.pending)
		return
	}
	c.seal(block)
}

// seal stores the facts of block, which has just executed.
//
// A transaction is kept only if it is in the final block, at its receipt position. That drops
// transactions the producer executed and then rolled back, whatever rule rolled them back. A
// block with any transaction missing is not stored, so the indexer falls back to
// debug_traceBlockByHash instead of reading a partial diff.
func (c *blockFactsCollector) seal(block *types.Block) {
	defer clear(c.pending)

	txs := block.Transactions()
	facts := make([]BlockFactsTx, len(txs))
	for i, tx := range txs {
		diff, ok := c.pending[tx.Hash()]
		if !ok {
			log.Error("blockfacts: transaction has no diff, block not served", "block", block.Hash(), "number", block.Number(), "tx", tx.Hash(), "index", i)
			blockFactsIncomplete.Inc(1)
			return
		}
		facts[i] = BlockFactsTx{TxHash: tx.Hash(), Result: diff}
	}

	hash := block.Hash()
	c.mu.Lock()
	defer c.mu.Unlock()
	// A reorg can rebuild a block already buffered; it keeps its ring slot.
	if _, ok := c.blocks[hash]; ok {
		c.blocks[hash] = facts
		blockFactsSealed.Inc(1)
		return
	}
	if evicted := c.ring[c.next]; evicted != (common.Hash{}) {
		delete(c.blocks, evicted)
	}
	c.ring[c.next] = hash
	c.next = (c.next + 1) % c.capacity
	c.blocks[hash] = facts
	blockFactsSealed.Inc(1)
}

// lookupBlockFacts returns the facts of the block with hash, or nil when it is not buffered.
func lookupBlockFacts(hash common.Hash) []BlockFactsTx {
	c := activeBlockFacts.Load()
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.blocks[hash]
}

// BlockFactsDebugAPI overrides debug_traceBlockByHash. It answers the indexer's prestate diff request from
// the sealed blocks, and sends every other request to geth's live tracer.
//
// Register it after geth's debug APIs: the RPC server keeps the last method registered under a
// name. It has exactly one exported method, so it replaces no other debug method.
type BlockFactsDebugAPI struct {
	live *tracers.API
}

// NewBlockFactsDebugAPI wraps live, the tracer API that serves every request the sealed blocks cannot.
func NewBlockFactsDebugAPI(live *tracers.API) *BlockFactsDebugAPI {
	return &BlockFactsDebugAPI{live: live}
}

// TraceBlockByHash returns the sealed diffs of the block with hash when config asks for exactly
// what the collector recorded, and a live trace otherwise. Both answers have the same shape.
func (a *BlockFactsDebugAPI) TraceBlockByHash(ctx context.Context, hash common.Hash, config *tracers.TraceConfig) (any, error) {
	if sealedTraceConfig(config) {
		if facts := lookupBlockFacts(hash); facts != nil {
			blockFactsServed.Inc(1)
			return facts, nil
		}
		blockFactsFallback.Inc(1)
	}
	return a.live.TraceBlockByHash(ctx, hash, config)
}

// sealedTraceConfig reports whether config asks for the prestate diff the collector records. The
// timeout is ignored, because a buffered answer needs no execution time.
func sealedTraceConfig(config *tracers.TraceConfig) bool {
	if config == nil || config.Tracer == nil || *config.Tracer != "prestateTracer" {
		return false
	}
	if config.Config != nil && *config.Config != (logger.Config{}) {
		return false
	}
	var got, want map[string]any
	if err := json.Unmarshal(config.TracerConfig, &got); err != nil {
		return false
	}
	if err := json.Unmarshal(blockFactsPrestateConfig, &want); err != nil {
		panic(fmt.Sprintf("blockfacts: blockFactsPrestateConfig is not valid JSON: %v", err))
	}
	return reflect.DeepEqual(got, want)
}
