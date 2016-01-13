// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.
package light

import (
	"math/big"
	"sync"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/logger"
	"github.com/ethereum/go-ethereum/logger/glog"
	"github.com/ethereum/go-ethereum/pow"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/hashicorp/golang-lru"
	"golang.org/x/net/context"
)

var (
	bodyCacheLimit      = 256
	blockCacheLimit     = 256
)

// LightChain represents the canonical chain given a database with a genesis
// block. The Blockchain manages chain imports, reverts, chain reorganisations.
//
// Importing blocks in to the block chain happens according to the set of rules
// defined by the two stage Validator. Processing of blocks is done using the
// Processor which processes the included transaction. The validation of the state
// is done in the second part of the Validator. Failing results in aborting of
// the import.
//
// The LightChain also helps in returning blocks from **any** chain included
// in the database as well as blocks that represents the canonical chain. It's
// important to note that GetBlock can return any block and does not need to be
// included in the canonical one where as GetBlockByNumber always represents the
// canonical chain.
type LightChain struct {
	hc           *core.HeaderChain
	chainDb      ethdb.Database
	odr			OdrBackend
	eventMux     *event.TypeMux
	genesisBlock *types.Block

	mu      sync.RWMutex
	chainmu sync.RWMutex
	procmu  sync.RWMutex

	bodyCache    *lru.Cache // Cache for the most recent block bodies
	bodyRLPCache *lru.Cache // Cache for the most recent block bodies in RLP encoded format
	blockCache   *lru.Cache // Cache for the most recent entire blocks

	quit    chan struct{}
	running int32 // running must be called automically
	// procInterrupt must be atomically called
	procInterrupt int32 // interrupt signaler for block processing
	wg            sync.WaitGroup

	pow       pow.PoW
	validator core.HeaderValidator
}

// NewLightChain returns a fully initialised block chain using information
// available in the database. It initialiser the default Ethereum Validator and
// Processor.
func NewLightChain(odr OdrBackend, pow pow.PoW, mux *event.TypeMux) (*LightChain, error) {
	bodyCache, _ := lru.New(bodyCacheLimit)
	bodyRLPCache, _ := lru.New(bodyCacheLimit)
	blockCache, _ := lru.New(blockCacheLimit)

	bc := &LightChain{
		chainDb:     odr.Database(),
		odr:			 odr,
		eventMux:     mux,
		quit:         make(chan struct{}),
		bodyCache:    bodyCache,
		bodyRLPCache: bodyRLPCache,
		blockCache:   blockCache,
		pow:          pow,
	}

	var err error
	bc.hc, err = core.NewHeaderChain(odr.Database(), bc.Validator, &bc.procInterrupt, &bc.wg, &bc.mu)
	bc.SetValidator(core.NewHdrValidator(bc.hc, pow))
	if err != nil {
		return nil, err
	}

	bc.genesisBlock, _ = bc.GetBlockByNumber(NoOdr, 0)
	if bc.genesisBlock == nil {
		bc.genesisBlock, err = core.WriteDefaultGenesisBlock(odr.Database())
		if err != nil {
			return nil, err
		}
		glog.V(logger.Info).Infoln("WARNING: Wrote default ethereum genesis block")
	}
	if err := bc.loadLastState(); err != nil {
		return nil, err
	}
	// Check the current state of the block hashes and make sure that we do not have any of the bad blocks in our chain
	for hash, _ := range core.BadHashes {
		if header := bc.GetHeader(hash); header != nil {
			glog.V(logger.Error).Infof("Found bad hash, rewinding chain to block #%d [%x…]", header.Number, header.ParentHash[:4])
			bc.SetHead(header.Number.Uint64() - 1)
			glog.V(logger.Error).Infoln("Chain rewind was successful, resuming normal operation")
		}
	}
	return bc, nil
}

// Odr returns the ODR backend of the chain
func (self *LightChain) Odr() OdrBackend {
	return self.odr
}

// loadLastState loads the last known chain state from the database. This method
// assumes that the chain manager mutex is held.
func (self *LightChain) loadLastState() error {
	if head := core.GetHeadHeaderHash(self.chainDb); head == (common.Hash{}) {
		// Corrupt or empty database, init from scratch
		self.Reset()
	} else {
		if header := self.GetHeader(head); header != nil {
			self.hc.SetCurrentHeader(header)
		}
	}

	// Issue a status log and return
	headerTd := self.GetTd(self.hc.CurrentHeader().Hash())
	glog.V(logger.Info).Infof("Last header: #%d [%x…] TD=%v", self.hc.CurrentHeader().Number, self.hc.CurrentHeader().Hash().Bytes()[:4], headerTd)

	return nil
}

// SetHead rewinds the local chain to a new head. In the case of headers, everything
// above the new head will be deleted and the new one set. In the case of blocks
// though, the head may be further rewound if block bodies are missing (non-archive
// nodes after a fast sync).
func (bc *LightChain) SetHead(head uint64) {
	bc.mu.Lock()
	defer bc.mu.Unlock()

	bc.hc.SetHead(head)
	bc.loadLastState()
}

// GasLimit returns the gas limit of the current HEAD block.
func (self *LightChain) GasLimit() *big.Int {
	self.mu.RLock()
	defer self.mu.RUnlock()

	return self.hc.CurrentHeader().GasLimit
}

// LastBlockHash return the hash of the HEAD block.
func (self *LightChain) LastBlockHash() common.Hash {
	self.mu.RLock()
	defer self.mu.RUnlock()

	return self.hc.CurrentHeader().Hash()
}

// Status returns status information about the current chain such as the HEAD Td,
// the HEAD hash and the hash of the genesis block.
func (self *LightChain) Status() (td *big.Int, currentBlock common.Hash, genesisBlock common.Hash) {
	self.mu.RLock()
	defer self.mu.RUnlock()

	hash := self.hc.CurrentHeader().Hash()
	return self.GetTd(hash), hash, self.genesisBlock.Hash()
}

// SetValidator sets the validator which is used to validate incoming blocks.
func (self *LightChain) SetValidator(validator core.HeaderValidator) {
	self.procmu.Lock()
	defer self.procmu.Unlock()
	self.validator = validator
}

// Validator returns the current validator.
func (self *LightChain) Validator() core.HeaderValidator {
	self.procmu.RLock()
	defer self.procmu.RUnlock()
	return self.validator
}

// State returns a new mutable state based on the current HEAD block.
func (self *LightChain) State() *LightState {
	return NewLightState(self.hc.CurrentHeader().Root, self.odr)
}

// Reset purges the entire blockchain, restoring it to its genesis state.
func (bc *LightChain) Reset() {
	bc.ResetWithGenesisBlock(bc.genesisBlock)
}

// ResetWithGenesisBlock purges the entire blockchain, restoring it to the
// specified genesis state.
func (bc *LightChain) ResetWithGenesisBlock(genesis *types.Block) {
	// Dump the entire block chain and purge the caches
	bc.SetHead(0)

	bc.mu.Lock()
	defer bc.mu.Unlock()

	// Prepare the genesis block and reinitialise the chain
	if err := core.WriteTd(bc.chainDb, genesis.Hash(), genesis.Difficulty()); err != nil {
		glog.Fatalf("failed to write genesis block TD: %v", err)
	}
	if err := core.WriteBlock(bc.chainDb, genesis); err != nil {
		glog.Fatalf("failed to write genesis block: %v", err)
	}
	bc.genesisBlock = genesis
	bc.hc.SetGenesis(bc.genesisBlock.Header())
	bc.hc.SetCurrentHeader(bc.genesisBlock.Header())
}


// Accessors
func (bc *LightChain) Genesis() *types.Block {
	return bc.genesisBlock
}

// GetBody retrieves a block body (transactions and uncles) from the database by
// hash, caching it if found.
func (self *LightChain) GetBody(ctx context.Context, hash common.Hash) (*types.Body, error) {
	// Short circuit if the body's already in the cache, retrieve otherwise
	if cached, ok := self.bodyCache.Get(hash); ok {
		body := cached.(*types.Body)
		return body, nil
	}
	body, err := GetBody(ctx, self.odr, hash)
	if err != nil {
		return nil, err
	}
	// Cache the found body for next time and return
	self.bodyCache.Add(hash, body)
	return body, nil
}

// GetBodyRLP retrieves a block body in RLP encoding from the database by hash,
// caching it if found.
func (self *LightChain) GetBodyRLP(ctx context.Context, hash common.Hash) (rlp.RawValue, error) {
	// Short circuit if the body's already in the cache, retrieve otherwise
	if cached, ok := self.bodyRLPCache.Get(hash); ok {
		return cached.(rlp.RawValue), nil
	}
	body, err := GetBodyRLP(ctx, self.odr, hash)
	if err != nil {
		return nil, err
	}
	// Cache the found body for next time and return
	self.bodyRLPCache.Add(hash, body)
	return body, nil
}

// HasBlock checks if a block is fully present in the database or not, caching
// it if present.
func (bc *LightChain) HasBlock(hash common.Hash) bool {
	blk, _ := bc.GetBlock(NoOdr, hash)
	return blk != nil
}

// GetBlock retrieves a block from the database by hash, caching it if found.
func (self *LightChain) GetBlock(ctx context.Context, hash common.Hash) (*types.Block, error) {
	// Short circuit if the block's already in the cache, retrieve otherwise
	if block, ok := self.blockCache.Get(hash); ok {
		return block.(*types.Block), nil
	}
	block, err := GetBlock(ctx, self.odr, hash)
	if err != nil {
		return nil, err
	}
	// Cache the found block for next time and return
	self.blockCache.Add(block.Hash(), block)
	return block, nil
}

// GetBlockByNumber retrieves a block from the database by number, caching it
// (associated with its hash) if found.
func (self *LightChain) GetBlockByNumber(ctx context.Context, number uint64) (*types.Block, error) {
	hash := core.GetCanonicalHash(self.chainDb, number)
	if hash == (common.Hash{}) {
		return nil, nil
	}
	return self.GetBlock(ctx, hash)
}

// Stop stops the blockchain service. If any imports are currently in progress
// it will abort them using the procInterrupt.
func (bc *LightChain) Stop() {
	if !atomic.CompareAndSwapInt32(&bc.running, 0, 1) {
		return
	}
	close(bc.quit)
	atomic.StoreInt32(&bc.procInterrupt, 1)

	bc.wg.Wait()

	glog.V(logger.Info).Infoln("Chain manager stopped")
}

type writeStatus byte

const (
	NonStatTy writeStatus = iota
	CanonStatTy
	SplitStatTy
	SideStatTy
)

// Rollback is designed to remove a chain of links from the database that aren't
// certain enough to be valid.
func (self *LightChain) Rollback(chain []common.Hash) {
	self.mu.Lock()
	defer self.mu.Unlock()

	self.hc.Rollback(chain)
}

/*
// postChainEvents iterates over the events generated by a chain insertion and
// posts them into the event mux.
func (self *LightChain) postChainEvents(events []interface{}, logs vm.Logs) {
	// post event logs for further processing
	self.eventMux.Post(logs)
	for _, event := range events {
		if event, ok := event.(ChainEvent); ok {
			if self.LastBlockHash() == event.Hash {
				self.eventMux.Post(ChainHeadEvent{event.Block})
			}
		}
		// Fire the insertion events individually too
		self.eventMux.Post(event)
	}
}*/

// InsertHeaderChain attempts to insert the given header chain in to the local
// chain, possibly creating a reorg. If an error is returned, it will return the
// index number of the failing header as well an error describing what went wrong.
//
// The verify parameter can be used to fine tune whether nonce verification
// should be done or not. The reason behind the optional check is because some
// of the header retrieval mechanisms already need to verfy nonces, as well as
// because nonces can be verified sparsely, not needing to check each.
func (self *LightChain) InsertHeaderChain(chain []*types.Header, checkFreq int) (int, error) {
	// Make sure only one thread manipulates the chain at once
	self.chainmu.Lock()
	defer self.chainmu.Unlock()

	return self.hc.InsertHeaderChain(chain, checkFreq)
}

// writeHeader writes a header into the local chain, given that its parent is
// already known. If the total difficulty of the newly inserted header becomes
// greater than the current known TD, the canonical chain is re-routed.
//
// Note: This method is not concurrent-safe with inserting blocks simultaneously
// into the chain, as side effects caused by reorganisations cannot be emulated
// without the real blocks. Hence, writing headers directly should only be done
// in two scenarios: pure-header mode of operation (light clients), or properly
// separated header/block phases (non-archive clients).
func (self *LightChain) writeHeader(header *types.Header) error {
	return self.hc.WriteHeader(header)
}

// CurrentHeader retrieves the current head header of the canonical chain. The
// header is retrieved from the HeaderChain's internal cache.
func (self *LightChain) CurrentHeader() *types.Header {
	self.mu.RLock()
	defer self.mu.RUnlock()

	return self.hc.CurrentHeader()
}

// GetTd retrieves a block's total difficulty in the canonical chain from the
// database by hash, caching it if found.
func (self *LightChain) GetTd(hash common.Hash) *big.Int {
	return self.hc.GetTd(hash)
}

// GetHeader retrieves a block header from the database by hash, caching it if
// found.
func (self *LightChain) GetHeader(hash common.Hash) *types.Header {
	return self.hc.GetHeader(hash)
}

// HasHeader checks if a block header is present in the database or not, caching
// it if present.
func (bc *LightChain) HasHeader(hash common.Hash) bool {
	return bc.hc.HasHeader(hash)
}

// GetBlockHashesFromHash retrieves a number of block hashes starting at a given
// hash, fetching towards the genesis block.
func (self *LightChain) GetBlockHashesFromHash(hash common.Hash, max uint64) []common.Hash {
	return self.hc.GetBlockHashesFromHash(hash, max)
}

// GetHeaderByNumber retrieves a block header from the database by number,
// caching it (associated with its hash) if found.
func (self *LightChain) GetHeaderByNumber(number uint64) *types.Header {
	return self.hc.GetHeaderByNumber(number)
}
