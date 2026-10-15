package live

import (
	"encoding/json"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth/tracers"
	"github.com/ethereum/go-ethereum/params"
)

// TestImportSealsEveryBlock imports a real chain with the tracer on and checks that each block is
// sealed with one diff per transaction, and that a diff carries the transfer it records.
func TestImportSealsEveryBlock(t *testing.T) {
	activeBlockFacts.Store(nil)
	t.Cleanup(func() { activeBlockFacts.Store(nil) })

	var (
		key, _    = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		sender    = crypto.PubkeyToAddress(key.PublicKey)
		recipient = common.HexToAddress("0x00000000000000000000000000000000000000aa")
		value     = big.NewInt(1000)
		config    = *params.AllEthashProtocolChanges
		genesis   = &core.Genesis{Config: &config, Alloc: types.GenesisAlloc{sender: {Balance: big.NewInt(params.Ether)}}}
		engine    = beacon.New(ethash.NewFaker())
		signer    = types.LatestSigner(&config)
	)

	hooks, err := tracers.LiveDirectory.New(BlockFactsName, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	options := core.DefaultConfig().WithStateScheme(rawdb.PathScheme)
	options.VmConfig = vm.Config{Tracer: hooks}
	chain, err := core.NewBlockChain(rawdb.NewMemoryDatabase(), genesis, engine, options)
	if err != nil {
		t.Fatal(err)
	}
	defer chain.Stop()

	nonce := uint64(0)
	_, blocks, _ := core.GenerateChainWithGenesis(genesis, engine, 3, func(i int, b *core.BlockGen) {
		for range 2 {
			tx, err := types.SignTx(types.NewTransaction(nonce, recipient, value, params.TxGas, b.BaseFee(), nil), signer, key)
			if err != nil {
				t.Fatal(err)
			}
			b.AddTx(tx)
			nonce++
		}
	})
	if n, err := chain.InsertChain(blocks); err != nil {
		t.Fatalf("block %d: %v", n, err)
	}

	for _, block := range blocks {
		facts := lookupBlockFacts(block.Hash())
		if len(facts) != len(block.Transactions()) {
			t.Fatalf("block %d: %d diffs for %d transactions", block.NumberU64(), len(facts), len(block.Transactions()))
		}
		for i, tx := range block.Transactions() {
			if facts[i].TxHash != tx.Hash() {
				t.Fatalf("block %d tx %d: diff for %s, want %s", block.NumberU64(), i, facts[i].TxHash, tx.Hash())
			}
		}
	}

	// The first transfer moves the recipient from nothing to value.
	var first struct {
		Post map[common.Address]struct {
			Balance *hexutil.Big `json:"balance"`
		} `json:"post"`
	}
	if err := json.Unmarshal(lookupBlockFacts(blocks[0].Hash())[0].Result, &first); err != nil {
		t.Fatal(err)
	}
	if got := first.Post[recipient].Balance; got == nil || got.ToInt().Cmp(value) != 0 {
		t.Fatalf("recipient post balance = %v, want %v", got, value)
	}
}
