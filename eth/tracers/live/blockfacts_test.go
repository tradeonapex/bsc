package live

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/tracers"
)

func newTestCollector(t *testing.T, capacity int) *blockFactsCollector {
	t.Helper()
	activeBlockFacts.Store(nil)
	t.Cleanup(func() { activeBlockFacts.Store(nil) })
	if _, err := newBlockFactsCollector(json.RawMessage(`{"capacity":` + big.NewInt(int64(capacity)).String() + `}`)); err != nil {
		t.Fatal(err)
	}
	return activeBlockFacts.Load()
}

func testBlock(number int64, nonces ...uint64) *types.Block {
	txs := make(types.Transactions, len(nonces))
	for i, n := range nonces {
		txs[i] = types.NewTx(&types.LegacyTx{Nonce: n, GasPrice: big.NewInt(1), Gas: 21000})
	}
	return types.NewBlockWithHeader(&types.Header{Number: big.NewInt(number)}).WithBody(types.Body{Transactions: txs})
}

func factsDiff(s string) json.RawMessage {
	return json.RawMessage(`{"post":{},"pre":{"` + s + `":{}}}`)
}

func TestSealKeepsOnlyBlockTransactionsInOrder(t *testing.T) {
	c := newTestCollector(t, 4)
	block := testBlock(1, 1, 2)
	txs := block.Transactions()
	c.pending[txs[1].Hash()] = factsDiff("b")
	c.pending[txs[0].Hash()] = factsDiff("a")
	c.pending[common.HexToHash("0xdead")] = factsDiff("rolled-back")

	sealViaHooks(c, block)

	got := lookupBlockFacts(block.Hash())
	if len(got) != 2 || got[0].TxHash != txs[0].Hash() || got[1].TxHash != txs[1].Hash() {
		t.Fatalf("facts not aligned with block order: %+v", got)
	}
	if len(c.pending) != 0 {
		t.Fatalf("pending not cleared: %d left", len(c.pending))
	}
}

func TestSealSkipsBlockWithMissingTransaction(t *testing.T) {
	c := newTestCollector(t, 4)
	block := testBlock(1, 1, 2)
	c.pending[block.Transactions()[0].Hash()] = factsDiff("a")

	sealViaHooks(c, block)

	if got := lookupBlockFacts(block.Hash()); got != nil {
		t.Fatalf("partial block served: %+v", got)
	}
	if len(c.pending) != 0 {
		t.Fatalf("pending not cleared: %d left", len(c.pending))
	}
}

func TestRingEvictsOldestAndKeepsResealedSlot(t *testing.T) {
	c := newTestCollector(t, 2)
	b1, b2, b3 := testBlock(1), testBlock(2), testBlock(3)

	sealViaHooks(c, b1)
	sealViaHooks(c, b2)
	sealViaHooks(c, b2) // a reorg rebuilt b2; it must not take a second slot
	sealViaHooks(c, b3)

	if lookupBlockFacts(b1.Hash()) != nil {
		t.Fatal("oldest block not evicted")
	}
	if lookupBlockFacts(b2.Hash()) == nil || lookupBlockFacts(b3.Hash()) == nil {
		t.Fatal("recent blocks missing")
	}
}

// traceConfig decodes body the way the RPC server decodes a debug_traceBlockByHash config.
func traceConfig(t *testing.T, body string) *tracers.TraceConfig {
	t.Helper()
	if body == "" {
		return nil
	}
	var cfg tracers.TraceConfig
	if err := json.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatal(err)
	}
	return &cfg
}

func TestSealedTraceConfigMatchesOnlyTheRecordedTrace(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"indexer request", `{"tracer":"prestateTracer","tracerConfig":{"diffMode":true,"disableCode":true}}`, true},
		{"key order and timeout do not matter", `{"tracer":"prestateTracer","timeout":"10s","tracerConfig":{"disableCode":true,"diffMode":true}}`, true},
		{"no config", ``, false},
		{"default tracer", `{}`, false},
		{"other tracer", `{"tracer":"callTracer","tracerConfig":{"diffMode":true,"disableCode":true}}`, false},
		{"not diff mode", `{"tracer":"prestateTracer","tracerConfig":{"disableCode":true}}`, false},
		{"with code", `{"tracer":"prestateTracer","tracerConfig":{"diffMode":true}}`, false},
		{"extra option", `{"tracer":"prestateTracer","tracerConfig":{"diffMode":true,"disableCode":true,"disableStorage":true}}`, false},
		{"chain overrides", `{"tracer":"prestateTracer","overrides":{"chainId":1},"tracerConfig":{"diffMode":true,"disableCode":true}}`, false},
	}
	for _, tc := range cases {
		if got := sealedTraceConfig(traceConfig(t, tc.body)); got != tc.want {
			t.Errorf("%s: sealedConfig = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestTraceBlockByHashServesSealedBlock(t *testing.T) {
	c := newTestCollector(t, 4)
	block := testBlock(1, 0, 1)
	for _, tx := range block.Transactions() {
		c.pending[tx.Hash()] = factsDiff(tx.Hash().Hex())
	}
	sealViaHooks(c, block)

	// live is nil: a request that reached it would panic, so a pass proves the buffer answered.
	api := NewBlockFactsDebugAPI(nil)
	got, err := api.TraceBlockByHash(context.Background(), block.Hash(), traceConfig(t, `{"tracer":"prestateTracer","tracerConfig":{"diffMode":true,"disableCode":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	facts, ok := got.([]BlockFactsTx)
	if !ok || len(facts) != 2 || facts[1].TxHash != block.Transactions()[1].Hash() {
		t.Fatalf("got %v, want the two sealed transactions in block order", got)
	}
}

// sealViaHooks runs block through the block hooks the chain fires on import.
func sealViaHooks(c *blockFactsCollector, block *types.Block) {
	if c == nil {
		return
	}
	pending := maps.Clone(c.pending)
	c.onBlockStart(tracing.BlockEvent{Block: block})
	maps.Copy(c.pending, pending)
	c.onBlockEnd(nil)
}

func TestFailedBlockIsNotStored(t *testing.T) {
	c := newTestCollector(t, 4)
	block := testBlock(1, 0)
	c.onBlockStart(tracing.BlockEvent{Block: block})
	c.pending[block.Transactions()[0].Hash()] = factsDiff("a")
	c.onBlockEnd(errors.New("bad block"))
	if lookupBlockFacts(block.Hash()) != nil {
		t.Fatal("a failed block was stored")
	}
	if len(c.pending) != 0 {
		t.Fatalf("pending not cleared: %d left", len(c.pending))
	}
}
