package kcoin

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"math/big"
	"os"
	"strings"
	"time"

	"github.com/kowala-tech/kcoin/common"
	"github.com/kowala-tech/kcoin/common/hexutil"
	"github.com/kowala-tech/kcoin/core"
	"github.com/kowala-tech/kcoin/core/state"
	"github.com/kowala-tech/kcoin/core/types"
	"github.com/kowala-tech/kcoin/core/vm"
	"github.com/kowala-tech/kcoin/internal/kcoinapi"
	"github.com/kowala-tech/kcoin/params"
	"github.com/kowala-tech/kcoin/rlp"
	"github.com/kowala-tech/kcoin/rpc"
	"github.com/kowala-tech/kcoin/trie"
)

const defaultTraceTimeout = 5 * time.Second

// PublicKowalaAPI provides an API to access Kowala full node-related
// information.
type PublicKowalaAPI struct {
	kcoin *Kowala
}

// NewPublicKowalaAPI creates a new Kowala protocol API for full nodes.
func NewPublicKowalaAPI(kcoin *Kowala) *PublicKowalaAPI {
	return &PublicKowalaAPI{kcoin}
}

// Coinbase is the address that consensus rewards will be send to
func (api *PublicKowalaAPI) Coinbase() (common.Address, error) {
	return api.kcoin.Coinbase()
}

// PrivateValidatorAPI provides private RPC methods to control the validator.
// These methods can be abused by external users and must be considered insecure for use by untrusted users.
type PrivateValidatorAPI struct {
	kcoin *Kowala
}

// NewPrivateValidatorAPI create a new RPC service which controls the validator of this node.
func NewPrivateValidatorAPI(kcoin *Kowala) *PrivateValidatorAPI {
	return &PrivateValidatorAPI{kcoin: kcoin}
}

// Start the validator.
func (api *PrivateValidatorAPI) Start() error {
	// Start the validator and return
	if !api.kcoin.IsValidating() {
		// Propagate the initial price point to the transaction pool
		api.kcoin.lock.RLock()
		price := api.kcoin.gasPrice
		api.kcoin.lock.RUnlock()

		api.kcoin.txPool.SetGasPrice(price)
		return api.kcoin.StartValidating()
	}
	return nil
}

// Stop the validator
func (api *PrivateValidatorAPI) Stop() bool {
	api.kcoin.StopValidating()
	return true
}

// SetExtra sets the extra data string that is included when this validator proposes a block.
func (api *PrivateValidatorAPI) SetExtra(extra string) (bool, error) {
	if err := api.kcoin.Validator().SetExtra([]byte(extra)); err != nil {
		return false, err
	}
	return true, nil
}

// SetGasPrice sets the minimum accepted gas price for the validator.
func (api *PrivateValidatorAPI) SetGasPrice(gasPrice hexutil.Big) bool {
	api.kcoin.lock.Lock()
	api.kcoin.gasPrice = (*big.Int)(&gasPrice)
	api.kcoin.lock.Unlock()

	api.kcoin.txPool.SetGasPrice((*big.Int)(&gasPrice))
	return true
}

// SetCoinbase sets the coinbase of the validator
func (api *PrivateValidatorAPI) SetCoinbase(coinbase common.Address) bool {
	api.kcoin.SetCoinbase(coinbase)
	return true
}

// SetDeposit sets the deposit of the validator
func (api *PrivateValidatorAPI) SetDeposit(deposit uint64) bool {
	api.kcoin.SetDeposit(deposit)
	return true
}

// GetMinimumDeposit gets the minimum deposit required to take a slot as a validator
func (api *PrivateValidatorAPI) GetMinimumDeposit() (uint64, error) {
	return api.kcoin.GetMinimumDeposit()
}

// GetDepositsResult is the result of a validator_getDeposits API call.
type GetDepositsResult struct {
	Deposits []depositEntry `json:"deposits"`
}

type depositEntry struct {
	Amount      uint64 `json:"value"`
	AvailableAt string `json:",omitempty"`
}

// GetDeposits returns the validator deposits
func (api *PrivateValidatorAPI) GetDeposits() (GetDepositsResult, error) {
	rawDeposits, err := api.kcoin.Validator().Deposits()
	if err != nil {
		return GetDepositsResult{}, err
	}
	deposits := make([]depositEntry, len(rawDeposits))
	for i, deposit := range rawDeposits {
		deposits[i] = depositEntry{
			Amount: deposit.Amount(),
		}
		// @NOTE (rgeraldes) - time.IsZero works in a different way
		if deposit.AvailableAt().Unix() == 0 {
			// @NOTE (rgeraldes) - zero values are not shown for this field
			deposits[i].AvailableAt = ""
		} else {
			deposits[i].AvailableAt = deposit.AvailableAt().String()
		}
	}

	return GetDepositsResult{Deposits: deposits}, nil
}

// RedeemDeposits requests a transfer of the unlocked deposits back
// to the validator account
func (api *PrivateValidatorAPI) RedeemDeposits() error {
	return api.kcoin.Validator().RedeemDeposits()
}

// PrivateAdminAPI is the collection of Kowala full node-related APIs
// exposed over the private admin endpoint.
type PrivateAdminAPI struct {
	kcoin *Kowala
}

// NewPrivateAdminAPI creates a new API definition for the full node private
// admin methods of the Kowala service.
func NewPrivateAdminAPI(kcoin *Kowala) *PrivateAdminAPI {
	return &PrivateAdminAPI{kcoin: kcoin}
}

// ExportChain exports the current blockchain into a local file.
func (api *PrivateAdminAPI) ExportChain(file string) (bool, error) {
	// Make sure we can create the file to export into
	out, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.ModePerm)
	if err != nil {
		return false, err
	}
	defer out.Close()

	var writer io.Writer = out
	if strings.HasSuffix(file, ".gz") {
		writer = gzip.NewWriter(writer)
		defer writer.(*gzip.Writer).Close()
	}

	// Export the blockchain
	if err := api.kcoin.BlockChain().Export(writer); err != nil {
		return false, err
	}
	return true, nil
}

func hasAllBlocks(chain *core.BlockChain, bs []*types.Block) bool {
	for _, b := range bs {
		if !chain.HasBlock(b.Hash(), b.NumberU64()) {
			return false
		}
	}

	return true
}

// ImportChain imports a blockchain from a local file.
func (api *PrivateAdminAPI) ImportChain(file string) (bool, error) {
	// Make sure the can access the file to import
	in, err := os.Open(file)
	if err != nil {
		return false, err
	}
	defer in.Close()

	var reader io.Reader = in
	if strings.HasSuffix(file, ".gz") {
		if reader, err = gzip.NewReader(reader); err != nil {
			return false, err
		}
	}

	// Run actual the import in pre-configured batches
	stream := rlp.NewStream(reader, 0)

	blocks, index := make([]*types.Block, 0, 2500), 0
	for batch := 0; ; batch++ {
		// Load a batch of blocks from the input file
		for len(blocks) < cap(blocks) {
			block := new(types.Block)
			if err := stream.Decode(block); err == io.EOF {
				break
			} else if err != nil {
				return false, fmt.Errorf("block %d: failed to parse: %v", index, err)
			}
			blocks = append(blocks, block)
			index++
		}
		if len(blocks) == 0 {
			break
		}

		if hasAllBlocks(api.kcoin.BlockChain(), blocks) {
			blocks = blocks[:0]
			continue
		}
		// Import the batch and reset the buffer
		if _, err := api.kcoin.BlockChain().InsertChain(blocks); err != nil {
			return false, fmt.Errorf("batch %d: failed to insert: %v", batch, err)
		}
		blocks = blocks[:0]
	}
	return true, nil
}

// PublicDebugAPI is the collection of Kowala full node APIs exposed
// over the public debugging endpoint.
type PublicDebugAPI struct {
	kcoin *Kowala
}

// NewPublicDebugAPI creates a new API definition for the full node-
// related public debug methods of the Kowala service.
func NewPublicDebugAPI(kcoin *Kowala) *PublicDebugAPI {
	return &PublicDebugAPI{kcoin: kcoin}
}

// DumpBlock retrieves the entire state of the database at a given block.
func (api *PublicDebugAPI) DumpBlock(blockNr rpc.BlockNumber) (state.Dump, error) {
	if blockNr == rpc.PendingBlockNumber {
		// If we're dumping the pending state, we need to request
		// both the pending block as well as the pending state from
		// the validator and operate on those
		_, stateDb := api.kcoin.validator.Pending()
		return stateDb.RawDump(), nil
	}
	var block *types.Block
	if blockNr == rpc.LatestBlockNumber {
		block = api.kcoin.blockchain.CurrentBlock()
	} else {
		block = api.kcoin.blockchain.GetBlockByNumber(uint64(blockNr))
	}
	if block == nil {
		return state.Dump{}, fmt.Errorf("block #%d not found", blockNr)
	}
	stateDb, err := api.kcoin.BlockChain().StateAt(block.Root())
	if err != nil {
		return state.Dump{}, err
	}
	return stateDb.RawDump(), nil
}

// PrivateDebugAPI is the collection of Kowala full node APIs exposed over
// the private debugging endpoint.
type PrivateDebugAPI struct {
	config *params.ChainConfig
	kcoin   *Kowala
}

// NewPrivateDebugAPI creates a new API definition for the full node-related
// private debug methods of the Kowala service.
func NewPrivateDebugAPI(config *params.ChainConfig, kcoin *Kowala) *PrivateDebugAPI {
	return &PrivateDebugAPI{config: config, kcoin: kcoin}
}

// BlockTraceResult is the returned value when replaying a block to check for
// consensus results and full VM trace logs for all included transactions.
type BlockTraceResult struct {
	Validated  bool                   `json:"validated"`
	StructLogs []kcoinapi.StructLogRes `json:"structLogs"`
	Error      string                 `json:"error"`
}

// TraceArgs holds extra parameters to trace functions
type TraceArgs struct {
	*vm.LogConfig
	Tracer  *string
	Timeout *string
}

// TraceBlock processes the given block'api RLP but does not import the block in to
// the chain.
func (api *PrivateDebugAPI) TraceBlock(blockRlp []byte, config *vm.LogConfig) BlockTraceResult {
	var block types.Block
	err := rlp.Decode(bytes.NewReader(blockRlp), &block)
	if err != nil {
		return BlockTraceResult{Error: fmt.Sprintf("could not decode block: %v", err)}
	}

	validated, logs, err := api.traceBlock(&block, config)
	return BlockTraceResult{
		Validated:  validated,
		StructLogs: kcoinapi.FormatLogs(logs),
		Error:      formatError(err),
	}
}

// TraceBlockFromFile loads the block'api RLP from the given file name and attempts to
// process it but does not import the block in to the chain.
func (api *PrivateDebugAPI) TraceBlockFromFile(file string, config *vm.LogConfig) BlockTraceResult {
	blockRlp, err := ioutil.ReadFile(file)
	if err != nil {
		return BlockTraceResult{Error: fmt.Sprintf("could not read file: %v", err)}
	}
	return api.TraceBlock(blockRlp, config)
}

// TraceBlockByNumber processes the block by canonical block number.
func (api *PrivateDebugAPI) TraceBlockByNumber(blockNr rpc.BlockNumber, config *vm.LogConfig) BlockTraceResult {
	// Fetch the block that we aim to reprocess
	var block *types.Block
	switch blockNr {
	case rpc.PendingBlockNumber:
		// Pending block is only known by the validator
		block = api.kcoin.validator.PendingBlock()
	case rpc.LatestBlockNumber:
		block = api.kcoin.blockchain.CurrentBlock()
	default:
		block = api.kcoin.blockchain.GetBlockByNumber(uint64(blockNr))
	}

	if block == nil {
		return BlockTraceResult{Error: fmt.Sprintf("block #%d not found", blockNr)}
	}

	validated, logs, err := api.traceBlock(block, config)
	return BlockTraceResult{
		Validated:  validated,
		StructLogs: kcoinapi.FormatLogs(logs),
		Error:      formatError(err),
	}
}

// TraceBlockByHash processes the block by hash.
func (api *PrivateDebugAPI) TraceBlockByHash(hash common.Hash, config *vm.LogConfig) BlockTraceResult {
	// Fetch the block that we aim to reprocess
	block := api.kcoin.BlockChain().GetBlockByHash(hash)
	if block == nil {
		return BlockTraceResult{Error: fmt.Sprintf("block #%x not found", hash)}
	}

	validated, logs, err := api.traceBlock(block, config)
	return BlockTraceResult{
		Validated:  validated,
		StructLogs: kcoinapi.FormatLogs(logs),
		Error:      formatError(err),
	}
}

// traceBlock processes the given block but does not save the state.
func (api *PrivateDebugAPI) traceBlock(block *types.Block, logConfig *vm.LogConfig) (bool, []vm.StructLog, error) {
	// Validate and reprocess the block
	var (
		blockchain = api.kcoin.BlockChain()
		validator  = blockchain.Validator()
		processor  = blockchain.Processor()
	)

	structLogger := vm.NewStructLogger(logConfig)

	config := vm.Config{
		Debug:  true,
		Tracer: structLogger,
	}
	if err := api.kcoin.engine.VerifyHeader(blockchain, block.Header(), true); err != nil {
		return false, structLogger.StructLogs(), err
	}
	statedb, err := blockchain.StateAt(blockchain.GetBlock(block.ParentHash(), block.NumberU64()-1).Root())
	if err != nil {
		return false, structLogger.StructLogs(), err
	}

	receipts, _, usedGas, err := processor.Process(block, statedb, config)
	if err != nil {
		return false, structLogger.StructLogs(), err
	}
	if err := validator.ValidateState(block, blockchain.GetBlock(block.ParentHash(), block.NumberU64()-1), statedb, receipts, usedGas); err != nil {
		return false, structLogger.StructLogs(), err
	}
	return true, structLogger.StructLogs(), nil
}

// formatError formats a Go error into either an empty string or the data content
// of the error itself.
func formatError(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

type timeoutError struct{}

func (t *timeoutError) Error() string {
	return "Execution time exceeded"
}

// TraceTransaction returns the structured logs created during the execution of EVM
// and returns them as a JSON object.
func (api *PrivateDebugAPI) TraceTransaction(ctx context.Context, txHash common.Hash, config *TraceArgs) (interface{}, error) {
	var tracer vm.Tracer
	if config != nil && config.Tracer != nil {
		timeout := defaultTraceTimeout
		if config.Timeout != nil {
			var err error
			if timeout, err = time.ParseDuration(*config.Timeout); err != nil {
				return nil, err
			}
		}

		var err error
		if tracer, err = kcoinapi.NewJavascriptTracer(*config.Tracer); err != nil {
			return nil, err
		}

		// Handle timeouts and RPC cancellations
		deadlineCtx, cancel := context.WithTimeout(ctx, timeout)
		go func() {
			<-deadlineCtx.Done()
			tracer.(*kcoinapi.JavascriptTracer).Stop(&timeoutError{})
		}()
		defer cancel()
	} else if config == nil {
		tracer = vm.NewStructLogger(nil)
	} else {
		tracer = vm.NewStructLogger(config.LogConfig)
	}

	// Retrieve the tx from the chain and the containing block
	tx, blockHash, _, txIndex := core.GetTransaction(api.kcoin.ChainDb(), txHash)
	if tx == nil {
		return nil, fmt.Errorf("transaction %x not found", txHash)
	}
	msg, context, statedb, err := api.computeTxEnv(blockHash, int(txIndex))
	if err != nil {
		return nil, err
	}

	// Run the transaction with tracing enabled.
	vmenv := vm.NewEVM(context, statedb, api.config, vm.Config{Debug: true, Tracer: tracer})
	ret, gas, failed, err := core.ApplyMessage(vmenv, msg, new(core.GasPool).AddGas(tx.Gas()))
	if err != nil {
		return nil, fmt.Errorf("tracing failed: %v", err)
	}
	switch tracer := tracer.(type) {
	case *vm.StructLogger:
		return &kcoinapi.ExecutionResult{
			Gas:         gas,
			Failed:      failed,
			ReturnValue: fmt.Sprintf("%x", ret),
			StructLogs:  kcoinapi.FormatLogs(tracer.StructLogs()),
		}, nil
	case *kcoinapi.JavascriptTracer:
		return tracer.GetResult()
	default:
		panic(fmt.Sprintf("bad tracer type %T", tracer))
	}
}

// computeTxEnv returns the execution environment of a certain transaction.
func (api *PrivateDebugAPI) computeTxEnv(blockHash common.Hash, txIndex int) (core.Message, vm.Context, *state.StateDB, error) {
	// Create the parent state.
	block := api.kcoin.BlockChain().GetBlockByHash(blockHash)
	if block == nil {
		return nil, vm.Context{}, nil, fmt.Errorf("block %x not found", blockHash)
	}
	parent := api.kcoin.BlockChain().GetBlock(block.ParentHash(), block.NumberU64()-1)
	if parent == nil {
		return nil, vm.Context{}, nil, fmt.Errorf("block parent %x not found", block.ParentHash())
	}
	statedb, err := api.kcoin.BlockChain().StateAt(parent.Root())
	if err != nil {
		return nil, vm.Context{}, nil, err
	}
	txs := block.Transactions()

	// Recompute transactions up to the target index.
	signer := types.MakeSigner(api.config, block.Number())
	for idx, tx := range txs {
		// Assemble the transaction call message
		msg, _ := tx.AsMessage(signer)
		context := core.NewEVMContext(msg, block.Header(), api.kcoin.BlockChain(), nil)
		if idx == txIndex {
			return msg, context, statedb, nil
		}

		vmenv := vm.NewEVM(context, statedb, api.config, vm.Config{})
		gp := new(core.GasPool).AddGas(tx.Gas())
		_, _, _, err := core.ApplyMessage(vmenv, msg, gp)
		if err != nil {
			return nil, vm.Context{}, nil, fmt.Errorf("tx %x failed: %v", tx.Hash(), err)
		}
		statedb.DeleteSuicides()
	}
	return nil, vm.Context{}, nil, fmt.Errorf("tx index %d out of range for block %x", txIndex, blockHash)
}

// Preimage is a debug API function that returns the preimage for a sha3 hash, if known.
func (api *PrivateDebugAPI) Preimage(ctx context.Context, hash common.Hash) (hexutil.Bytes, error) {
	db := core.PreimageTable(api.kcoin.ChainDb())
	return db.Get(hash.Bytes())
}

// GetBadBLocks returns a list of the last 'bad blocks' that the client has seen on the network
// and returns them as a JSON list of block-hashes
func (api *PrivateDebugAPI) GetBadBlocks(ctx context.Context) ([]core.BadBlockArgs, error) {
	return api.kcoin.BlockChain().BadBlocks()
}

// StorageRangeResult is the result of a debug_storageRangeAt API call.
type StorageRangeResult struct {
	Storage storageMap   `json:"storage"`
	NextKey *common.Hash `json:"nextKey"` // nil if Storage includes the last key in the trie.
}

type storageMap map[common.Hash]storageEntry

type storageEntry struct {
	Key   *common.Hash `json:"key"`
	Value common.Hash  `json:"value"`
}

// StorageRangeAt returns the storage at the given block height and transaction index.
func (api *PrivateDebugAPI) StorageRangeAt(ctx context.Context, blockHash common.Hash, txIndex int, contractAddress common.Address, keyStart hexutil.Bytes, maxResult int) (StorageRangeResult, error) {
	_, _, statedb, err := api.computeTxEnv(blockHash, txIndex)
	if err != nil {
		return StorageRangeResult{}, err
	}
	st := statedb.StorageTrie(contractAddress)
	if st == nil {
		return StorageRangeResult{}, fmt.Errorf("account %x doesn't exist", contractAddress)
	}
	return storageRangeAt(st, keyStart, maxResult), nil
}

func storageRangeAt(st state.Trie, start []byte, maxResult int) StorageRangeResult {
	it := trie.NewIterator(st.NodeIterator(start))
	result := StorageRangeResult{Storage: storageMap{}}
	for i := 0; i < maxResult && it.Next(); i++ {
		e := storageEntry{Value: common.BytesToHash(it.Value)}
		if preimage := st.GetKey(it.Key); preimage != nil {
			preimage := common.BytesToHash(preimage)
			e.Key = &preimage
		}
		result.Storage[common.BytesToHash(it.Key)] = e
	}
	// Add the 'next key' so clients can continue downloading.
	if it.Next() {
		next := common.BytesToHash(it.Key)
		result.NextKey = &next
	}
	return result
}

// GetModifiedAccountsByumber returns all accounts that have changed between the
// two blocks specified. A change is defined as a difference in nonce, balance,
// code hash, or storage hash.
//
// With one parameter, returns the list of accounts modified in the specified block.
func (api *PrivateDebugAPI) GetModifiedAccountsByNumber(startNum uint64, endNum *uint64) ([]common.Address, error) {
	var startBlock, endBlock *types.Block

	startBlock = api.kcoin.blockchain.GetBlockByNumber(startNum)
	if startBlock == nil {
		return nil, fmt.Errorf("start block %x not found", startNum)
	}

	if endNum == nil {
		endBlock = startBlock
		startBlock = api.kcoin.blockchain.GetBlockByHash(startBlock.ParentHash())
		if startBlock == nil {
			return nil, fmt.Errorf("block %x has no parent", endBlock.Number())
		}
	} else {
		endBlock = api.kcoin.blockchain.GetBlockByNumber(*endNum)
		if endBlock == nil {
			return nil, fmt.Errorf("end block %d not found", *endNum)
		}
	}
	return api.getModifiedAccounts(startBlock, endBlock)
}

// GetModifiedAccountsByHash returns all accounts that have changed between the
// two blocks specified. A change is defined as a difference in nonce, balance,
// code hash, or storage hash.
//
// With one parameter, returns the list of accounts modified in the specified block.
func (api *PrivateDebugAPI) GetModifiedAccountsByHash(startHash common.Hash, endHash *common.Hash) ([]common.Address, error) {
	var startBlock, endBlock *types.Block
	startBlock = api.kcoin.blockchain.GetBlockByHash(startHash)
	if startBlock == nil {
		return nil, fmt.Errorf("start block %x not found", startHash)
	}

	if endHash == nil {
		endBlock = startBlock
		startBlock = api.kcoin.blockchain.GetBlockByHash(startBlock.ParentHash())
		if startBlock == nil {
			return nil, fmt.Errorf("block %x has no parent", endBlock.Number())
		}
	} else {
		endBlock = api.kcoin.blockchain.GetBlockByHash(*endHash)
		if endBlock == nil {
			return nil, fmt.Errorf("end block %x not found", *endHash)
		}
	}
	return api.getModifiedAccounts(startBlock, endBlock)
}

func (api *PrivateDebugAPI) getModifiedAccounts(startBlock, endBlock *types.Block) ([]common.Address, error) {
	if startBlock.Number().Uint64() >= endBlock.Number().Uint64() {
		return nil, fmt.Errorf("start block height (%d) must be less than end block height (%d)", startBlock.Number().Uint64(), endBlock.Number().Uint64())
	}

	oldTrie, err := trie.NewSecure(startBlock.Root(), api.kcoin.chainDb, 0)
	if err != nil {
		return nil, err
	}
	newTrie, err := trie.NewSecure(endBlock.Root(), api.kcoin.chainDb, 0)
	if err != nil {
		return nil, err
	}

	diff, _ := trie.NewDifferenceIterator(oldTrie.NodeIterator([]byte{}), newTrie.NodeIterator([]byte{}))
	iter := trie.NewIterator(diff)

	var dirty []common.Address
	for iter.Next() {
		key := newTrie.GetKey(iter.Key)
		if key == nil {
			return nil, fmt.Errorf("no preimage found for hash %x", iter.Key)
		}
		dirty = append(dirty, common.BytesToAddress(key))
	}
	return dirty, nil
}