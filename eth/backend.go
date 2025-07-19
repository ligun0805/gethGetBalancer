// Copyright 2014 The go-ethereum Authors
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

// Package eth implements the Ethereum protocol.
package eth

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/holiman/uint256"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/filtermaps"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/state/pruner"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/txpool/blobpool"
	"github.com/ethereum/go-ethereum/core/txpool/legacypool"
	"github.com/ethereum/go-ethereum/core/txpool/locals"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth/downloader"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/eth/gasprice"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/eth/protocols/snap"
	"github.com/ethereum/go-ethereum/eth/tracers"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/internal/shutdowncheck"
	"github.com/ethereum/go-ethereum/internal/version"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/miner"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/dnsdisc"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/rpc"
	gethversion "github.com/ethereum/go-ethereum/version"
)

// Config contains the configuration options of the ETH protocol.
// Deprecated: use ethconfig.Config instead.
type Config = ethconfig.Config

// Ethereum implements the Ethereum full node service.
type Ethereum struct {
	// core protocol objects
	config         *ethconfig.Config
	txPool         *txpool.TxPool
	localTxTracker *locals.TxTracker
	blockchain     *core.BlockChain

	handler *handler
	discmix *enode.FairMix
	dropper *dropper

	// DB interfaces
	chainDb ethdb.Database // Block chain database

	eventMux       *event.TypeMux
	engine         consensus.Engine
	accountManager *accounts.Manager

	filterMaps      *filtermaps.FilterMaps
	closeFilterMaps chan chan struct{}

	APIBackend *EthAPIBackend

	miner    *miner.Miner
	gasPrice *big.Int

	networkID     uint64
	netRPCService *ethapi.NetAPI

	p2pServer *p2p.Server

	lock sync.RWMutex // Protects the variadic fields (e.g. gas price and etherbase)

	shutdownTracker *shutdowncheck.ShutdownTracker // Tracks if and when the node has shutdown ungracefully

	// *** Added fields for address monitoring and token sending ***
	filterData      []byte                      // Bloom filter bit array for quick address lookup
	filterBits      int                         // Number of bits in the Bloom filter
	sentAddresses   map[common.Address]struct{} // Set of addresses that have been rewarded
	sentLog         *os.File                    // Log file for addresses to which tokens were sent
	tokenContract   common.Address              // ERC20 token contract address (loaded from .env)
	tokenSenderKey  *ecdsa.PrivateKey           // Private key for sending the ERC20 token (from .env)
	tokenSenderAddr common.Address              // Address corresponding to the private key
}

// New creates a new Ethereum object (including the initialisation of the common Ethereum object),
// whose lifecycle will be managed by the provided node.
func New(stack *node.Node, config *ethconfig.Config) (*Ethereum, error) {
	// Ensure configuration values are compatible and sane
	if !config.SyncMode.IsValid() {
		return nil, fmt.Errorf("invalid sync mode %d", config.SyncMode)
	}
	if !config.HistoryMode.IsValid() {
		return nil, fmt.Errorf("invalid history mode %d", config.HistoryMode)
	}
	if config.Miner.GasPrice == nil || config.Miner.GasPrice.Sign() <= 0 {
		log.Warn("Sanitizing invalid miner gas price", "provided", config.Miner.GasPrice, "updated", ethconfig.Defaults.Miner.GasPrice)
		config.Miner.GasPrice = new(big.Int).Set(ethconfig.Defaults.Miner.GasPrice)
	}
	if config.NoPruning && config.TrieDirtyCache > 0 {
		if config.SnapshotCache > 0 {
			config.TrieCleanCache += config.TrieDirtyCache * 3 / 5
			config.SnapshotCache += config.TrieDirtyCache * 2 / 5
		} else {
			config.TrieCleanCache += config.TrieDirtyCache
		}
		config.TrieDirtyCache = 0
	}
	log.Info("Allocated trie memory caches", "clean", common.StorageSize(config.TrieCleanCache)*1024*1024, "dirty", common.StorageSize(config.TrieDirtyCache)*1024*1024)

	chainDb, err := stack.OpenDatabaseWithFreezer("chaindata", config.DatabaseCache, config.DatabaseHandles, config.DatabaseFreezer, "eth/db/chaindata/", false)
	if err != nil {
		return nil, err
	}
	scheme, err := rawdb.ParseStateScheme(config.StateScheme, chainDb)
	if err != nil {
		return nil, err
	}
	// Try to recover offline state pruning only in hash-based.
	if scheme == rawdb.HashScheme {
		if err := pruner.RecoverPruning(stack.ResolvePath(""), chainDb); err != nil {
			log.Error("Failed to recover state", "error", err)
		}
	}

	// Here we determine genesis hash and active ChainConfig.
	// We need these to figure out the consensus parameters and to set up history pruning.
	chainConfig, _, err := core.LoadChainConfig(chainDb, config.Genesis)
	if err != nil {
		return nil, err
	}
	engine, err := ethconfig.CreateConsensusEngine(chainConfig, chainDb)
	if err != nil {
		return nil, err
	}
	// Set networkID to chainID by default.
	networkID := config.NetworkId
	if networkID == 0 {
		networkID = chainConfig.ChainID.Uint64()
	}

	// Assemble the Ethereum object.
	eth := &Ethereum{
		config:          config,
		chainDb:         chainDb,
		eventMux:        stack.EventMux(),
		accountManager:  stack.AccountManager(),
		engine:          engine,
		networkID:       networkID,
		gasPrice:        config.Miner.GasPrice,
		p2pServer:       stack.Server(),
		discmix:         enode.NewFairMix(0),
		shutdownTracker: shutdowncheck.NewShutdownTracker(chainDb),
	}
	bcVersion := rawdb.ReadDatabaseVersion(chainDb)
	var dbVer = "<nil>"
	if bcVersion != nil {
		dbVer = fmt.Sprintf("%d", *bcVersion)
	}
	log.Info("Initialising Ethereum protocol", "network", networkID, "dbversion", dbVer)

	// Create BlockChain object.
	if !config.SkipBcVersionCheck {
		if bcVersion != nil && *bcVersion > core.BlockChainVersion {
			return nil, fmt.Errorf("database version is v%d, Geth %s only supports v%d", *bcVersion, version.WithMeta, core.BlockChainVersion)
		} else if bcVersion == nil || *bcVersion < core.BlockChainVersion {
			if bcVersion != nil { // only print warning on upgrade, not on init
				log.Warn("Upgrade blockchain database version", "from", dbVer, "to", core.BlockChainVersion)
			}
			rawdb.WriteDatabaseVersion(chainDb, core.BlockChainVersion)
		}
	}
	var (
		vmConfig = vm.Config{
			EnablePreimageRecording: config.EnablePreimageRecording,
		}
		cacheConfig = &core.CacheConfig{
			TrieCleanLimit:      config.TrieCleanCache,
			TrieCleanNoPrefetch: config.NoPrefetch,
			TrieDirtyLimit:      config.TrieDirtyCache,
			TrieDirtyDisabled:   config.NoPruning,
			TrieTimeLimit:       config.TrieTimeout,
			SnapshotLimit:       config.SnapshotCache,
			Preimages:           config.Preimages,
			StateHistory:        config.StateHistory,
			StateScheme:         scheme,
			ChainHistoryMode:    config.HistoryMode,
		}
	)
	if config.VMTrace != "" {
		traceConfig := json.RawMessage("{}")
		if config.VMTraceJsonConfig != "" {
			traceConfig = json.RawMessage(config.VMTraceJsonConfig)
		}
		t, err := tracers.LiveDirectory.New(config.VMTrace, traceConfig)
		if err != nil {
			return nil, fmt.Errorf("failed to create tracer %s: %v", config.VMTrace, err)
		}
		vmConfig.Tracer = t
	}
	// Override the chain config with provided settings.
	var overrides core.ChainOverrides
	if config.OverridePrague != nil {
		overrides.OverridePrague = config.OverridePrague
	}
	if config.OverrideVerkle != nil {
		overrides.OverrideVerkle = config.OverrideVerkle
	}
	eth.blockchain, err = core.NewBlockChain(chainDb, cacheConfig, config.Genesis, &overrides, eth.engine, vmConfig, &config.TransactionHistory)
	if err != nil {
		return nil, err
	}

	// Initialize filtermaps log index.
	fmConfig := filtermaps.Config{
		History:        config.LogHistory,
		Disabled:       config.LogNoHistory,
		ExportFileName: config.LogExportCheckpoints,
		HashScheme:     scheme == rawdb.HashScheme,
	}
	chainView := eth.newChainView(eth.blockchain.CurrentBlock())
	historyCutoff, _ := eth.blockchain.HistoryPruningCutoff()
	var finalBlock uint64
	if fb := eth.blockchain.CurrentFinalBlock(); fb != nil {
		finalBlock = fb.Number.Uint64()
	}
	eth.filterMaps = filtermaps.NewFilterMaps(chainDb, chainView, historyCutoff, finalBlock, filtermaps.DefaultParams, fmConfig)
	eth.closeFilterMaps = make(chan chan struct{})

	// TxPool
	if config.TxPool.Journal != "" {
		_ = os.Remove(stack.ResolvePath(config.TxPool.Journal))
	}
	legacyPool := legacypool.New(config.TxPool, eth.blockchain)

	if config.BlobPool.Datadir != "" {
		config.BlobPool.Datadir = stack.ResolvePath(config.BlobPool.Datadir)
	}
	blobPool := blobpool.New(config.BlobPool, eth.blockchain, legacyPool.HasPendingAuth)

	eth.txPool, err = txpool.New(config.TxPool.PriceLimit, eth.blockchain, []txpool.SubPool{legacyPool, blobPool})
	if err != nil {
		return nil, err
	}

	if !config.TxPool.NoLocals {
		rejournal := config.TxPool.Rejournal
		if rejournal < time.Second {
			log.Warn("Sanitizing invalid txpool journal time", "provided", rejournal, "updated", time.Second)
			rejournal = time.Second
		}
		eth.localTxTracker = locals.New(config.TxPool.Journal, rejournal, eth.blockchain.Config(), eth.txPool)
		stack.RegisterLifecycle(eth.localTxTracker)
	}

	// Permit the downloader to use the trie cache allowance during fast sync
	cacheLimit := cacheConfig.TrieCleanLimit + cacheConfig.TrieDirtyLimit + cacheConfig.SnapshotLimit
	if eth.handler, err = newHandler(&handlerConfig{
		NodeID:         eth.p2pServer.Self().ID(),
		Database:       chainDb,
		Chain:          eth.blockchain,
		TxPool:         eth.txPool,
		Network:        networkID,
		Sync:           config.SyncMode,
		BloomCache:     uint64(cacheLimit),
		EventMux:       eth.eventMux,
		RequiredBlocks: config.RequiredBlocks,
	}); err != nil {
		return nil, err
	}

	eth.dropper = newDropper(eth.p2pServer.MaxDialedConns(), eth.p2pServer.MaxInboundConns())

	eth.miner = miner.New(eth, config.Miner, eth.engine)
	eth.miner.SetExtra(makeExtraData(config.Miner.ExtraData))
	eth.miner.SetPrioAddresses(config.TxPool.Locals)

	eth.APIBackend = &EthAPIBackend{stack.Config().ExtRPCEnabled(), stack.Config().AllowUnprotectedTxs, eth, nil}
	if eth.APIBackend.allowUnprotectedTxs {
		log.Info("Unprotected transactions allowed")
	}
	eth.APIBackend.gpo = gasprice.NewOracle(eth.APIBackend, config.GPO, config.Miner.GasPrice)

	// Start the RPC service
	eth.netRPCService = ethapi.NewNetAPI(eth.p2pServer, networkID)

	// Register the backend on the node
	stack.RegisterAPIs(eth.APIs())
	stack.RegisterProtocols(eth.Protocols())
	stack.RegisterLifecycle(eth)

	//1. Loading configuration variables from .env file
	envFile, err := os.Open(".env")
	if err == nil {
		scanner := bufio.NewScanner(envFile)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue // skip blank lines and comments
			}
			parts := strings.SplitN(line, "=", 2)
			if len(parts) != 2 {
				continue
			}
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			// Remove framing quotation marks if any
			if len(val) > 1 && ((val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'')) {
				val = val[1 : len(val)-1]
			}
			os.Setenv(key, val)
		}
		envFile.Close()
		log.Info("Loaded environment variables from .env file")
	}

	// 2. Loading/building a Bloom filter of addresses
	addressesFile := os.Getenv("ADDRESSES_FILE")
	if addressesFile == "" {
		addressesFile = "addresses.txt"
	}
	var filterLoaded bool
	if statFilter, err := os.Stat("filter.bin"); err == nil {
		// The filter file exists - check its relevance
		if statAddrs, err := os.Stat(addressesFile); err != nil || !statAddrs.ModTime().After(statFilter.ModTime()) {
			data, err := os.ReadFile("filter.bin")
			if err == nil {
				eth.filterData = data
				eth.filterBits = len(data) * 8
				filterLoaded = true
				log.Info("Bloom filter loaded from file", "bits", eth.filterBits)
			}
		}
	}
	if !filterLoaded {
		file, err := os.Open(addressesFile)
		if err != nil {
			log.Error("Failed to open address file", "err", err)
		} else {
			var addresses []common.Address
			scanner := bufio.NewScanner(file)
			for scanner.Scan() {
				line := strings.TrimSpace(scanner.Text())
				if line == "" {
					continue
				}
				addresses = append(addresses, common.HexToAddress(line))
			}
			file.Close()
			if len(addresses) > 0 {
				eth.filterBits = len(addresses) * 16
				if eth.filterBits < 2048 {
					eth.filterBits = 2048
				}
				// equalize the number of bits to an integer number of bytes
				if eth.filterBits%8 != 0 {
					eth.filterBits += 8 - (eth.filterBits % 8)
				}
				eth.filterData = make([]byte, eth.filterBits/8)
				// Filling the Bloom filter: for each address, set 3 bits by hashes
				for _, addr := range addresses {
					hash := crypto.Keccak256(addr.Bytes())
					for i := 0; i < 3; i++ {
						idx := binary.LittleEndian.Uint16(hash[2*i:2*i+2]) % uint16(eth.filterBits)
						eth.filterData[idx/8] |= 1 << (idx % 8)
					}
				}
				// Save the filter for reuse
				if err := os.WriteFile("filter.bin", eth.filterData, 0644); err != nil {
					log.Error("Failed to write Bloom filter to file", "err", err)
				}
				log.Info("Bloom filter initialized", "addresses", len(addresses), "bits", eth.filterBits)
			}
		}
	}

	// 3. Load token contract address and sender private key from environment
	tokenAddrHex := os.Getenv("TOKEN_CONTRACT")
	privKeyHex := os.Getenv("TOKEN_PRIVATE_KEY")
	if tokenAddrHex == "" || privKeyHex == "" {
		log.Error("Token contract address or private key not set in environment")
	} else {
		eth.tokenContract = common.HexToAddress(tokenAddrHex)
		key, err := crypto.HexToECDSA(strings.TrimPrefix(privKeyHex, "0x"))
		if err != nil {
			log.Error("Invalid private key for token sender", "err", err)
		} else {
			eth.tokenSenderKey = key
			eth.tokenSenderAddr = crypto.PubkeyToAddress(key.PublicKey)
			log.Info("Token sender configured", "address", eth.tokenSenderAddr.Hex(), "tokenContract", eth.tokenContract.Hex())
		}
	}

	// Warn if token sender has no Ether (cannot pay gas fees)
	if eth.tokenSenderAddr != (common.Address{}) {
		header := eth.blockchain.CurrentHeader()
		if state, err := eth.blockchain.StateAt(header.Root); err == nil {
			balance := state.GetBalance(eth.tokenSenderAddr)
			if balance.Sign() == 0 {
				log.Warn("Token sender address has zero Ether balance; it cannot cover gas fees for token transfers")
			}
		}
	}

	// 4. Open or create log file for sent addresses, and load any existing entries into the set
	eth.sentAddresses = make(map[common.Address]struct{})
	logFile, err := os.OpenFile("sent.log", os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
	if err != nil {
		log.Error("Could not open sent.log file", "err", err)
	} else {
		eth.sentLog = logFile
		scanner := bufio.NewScanner(logFile)
		for scanner.Scan() {
			addrHex := strings.TrimSpace(scanner.Text())
			if addrHex == "" {
				continue
			}
			addr := common.HexToAddress(addrHex)
			eth.sentAddresses[addr] = struct{}{}
		}
		// Move the file pointer to the end for subsequent addition of records
		_, _ = logFile.Seek(0, os.SEEK_END)
	}

	// Successful startup; push a marker and check previous unclean shutdowns.
	eth.shutdownTracker.MarkStartup()

	return eth, nil
}

func makeExtraData(extra []byte) []byte {
	if len(extra) == 0 {
		// create default extradata
		extra, _ = rlp.EncodeToBytes([]interface{}{
			uint(gethversion.Major<<16 | gethversion.Minor<<8 | gethversion.Patch),
			"geth",
			runtime.Version(),
			runtime.GOOS,
		})
	}
	if uint64(len(extra)) > params.MaximumExtraDataSize {
		log.Warn("Miner extra data exceed limit", "extra", hexutil.Bytes(extra), "limit", params.MaximumExtraDataSize)
		extra = nil
	}
	return extra
}

// APIs return the collection of RPC services the ethereum package offers.
// NOTE, some of these services probably need to be moved to somewhere else.
func (s *Ethereum) APIs() []rpc.API {
	apis := ethapi.GetAPIs(s.APIBackend)

	// Append any APIs exposed explicitly by the consensus engine
	apis = append(apis, s.engine.APIs(s.BlockChain())...)

	// Append all the local APIs and return
	return append(apis, []rpc.API{
		{
			Namespace: "miner",
			Service:   NewMinerAPI(s),
		}, {
			Namespace: "eth",
			Service:   downloader.NewDownloaderAPI(s.handler.downloader, s.blockchain, s.eventMux),
		}, {
			Namespace: "admin",
			Service:   NewAdminAPI(s),
		}, {
			Namespace: "debug",
			Service:   NewDebugAPI(s),
		}, {
			Namespace: "net",
			Service:   s.netRPCService,
		},
	}...)
}

func (s *Ethereum) ResetWithGenesisBlock(gb *types.Block) {
	s.blockchain.ResetWithGenesisBlock(gb)
}

func (s *Ethereum) Miner() *miner.Miner { return s.miner }

func (s *Ethereum) AccountManager() *accounts.Manager  { return s.accountManager }
func (s *Ethereum) BlockChain() *core.BlockChain       { return s.blockchain }
func (s *Ethereum) TxPool() *txpool.TxPool             { return s.txPool }
func (s *Ethereum) Engine() consensus.Engine           { return s.engine }
func (s *Ethereum) ChainDb() ethdb.Database            { return s.chainDb }
func (s *Ethereum) IsListening() bool                  { return true } // Always listening
func (s *Ethereum) Downloader() *downloader.Downloader { return s.handler.downloader }
func (s *Ethereum) Synced() bool                       { return s.handler.synced.Load() }
func (s *Ethereum) SetSynced()                         { s.handler.enableSyncedFeatures() }
func (s *Ethereum) ArchiveMode() bool                  { return s.config.NoPruning }

// Protocols returns all the currently configured
// network protocols to start.
func (s *Ethereum) Protocols() []p2p.Protocol {
	protos := eth.MakeProtocols((*ethHandler)(s.handler), s.networkID, s.discmix)
	if s.config.SnapshotCache > 0 {
		protos = append(protos, snap.MakeProtocols((*snapHandler)(s.handler))...)
	}
	return protos
}

// Start implements node.Lifecycle, starting all internal goroutines needed by the
// Ethereum protocol implementation.
func (s *Ethereum) Start() error {
	if err := s.setupDiscovery(); err != nil {
		return err
	}

	// Regularly update shutdown marker
	s.shutdownTracker.Start()

	// 5. Subscribe to new pending transactions from the transaction pool
	// Get all current transactions that are already in the mempool
	txCh := make(chan core.NewTxsEvent, 1000)
	subscriptionStart := time.Now()
	sub := s.txPool.SubscribeTransactions(txCh, false)

	// Clear the original transaction queue
	go func() {
		defer sub.Unsubscribe()

		// Now start processing only new transactions
		log.Info("Subscribed to txPool; processing only NEW transactions")

		for txEvent := range txCh {
			header := s.blockchain.CurrentHeader()
			stateAtHead, _ := s.blockchain.StateAt(header.Root)
			for i, tx := range txEvent.Txs {
				if txEvent.Times[i].Before(subscriptionStart) {
					continue
				}
				signer := types.LatestSigner(s.blockchain.Config())
				from, err := types.Sender(signer, tx)
				if err != nil {
					log.Error("Failed to derive tx sender", "err", err, "txHash", tx.Hash().Hex())
					continue
				}
				if from == s.tokenSenderAddr {
					continue
				}

				origHash := tx.Hash()
				var affectedAddrs []common.Address
				if tx.To() != nil && tx.Value().Sign() > 0 && stateAtHead != nil {
					code := stateAtHead.GetCode(*tx.To())
					if len(code) == 0 {
						affectedAddrs = append(affectedAddrs, *tx.To())
					}
				}

				if len(affectedAddrs) == 0 {
					affectedAddrs = s.simulateTxGetGains(tx)
				}

				for _, addr := range affectedAddrs {
					if s.filterData == nil || s.filterBits == 0 {
						continue
					}
					hash := crypto.Keccak256(addr.Bytes())
					hit := true
					for i := 0; i < 3; i++ {
						idx := binary.LittleEndian.Uint16(hash[2*i:2*i+2]) % uint16(s.filterBits)
						if s.filterData[idx/8]&(1<<(idx%8)) == 0 {
							hit = false
							break
						}
					}
					if !hit {
						continue
					}
					if _, sent := s.sentAddresses[addr]; sent {
						continue
					}
					if err := s.sendToken(addr, origHash); err != nil {
						log.Error("Failed to send token", "to", addr.Hex(), "origTx", origHash.Hex(), "err", err)
					}
				}
			}
		}
	}()

	// Start the networking layer
	s.handler.Start(s.p2pServer.MaxPeers)

	// Start the connection manager
	s.dropper.Start(s.p2pServer, func() bool { return !s.Synced() })

	// start log indexer
	s.filterMaps.Start()
	go s.updateFilterMapsHeads()
	return nil
}

func (s *Ethereum) newChainView(head *types.Header) *filtermaps.ChainView {
	if head == nil {
		return nil
	}
	return filtermaps.NewChainView(s.blockchain, head.Number.Uint64(), head.Hash())
}

func (s *Ethereum) updateFilterMapsHeads() {
	headEventCh := make(chan core.ChainEvent, 10)
	blockProcCh := make(chan bool, 10)
	sub := s.blockchain.SubscribeChainEvent(headEventCh)
	sub2 := s.blockchain.SubscribeBlockProcessingEvent(blockProcCh)
	defer func() {
		sub.Unsubscribe()
		sub2.Unsubscribe()
		for {
			select {
			case <-headEventCh:
			case <-blockProcCh:
			default:
				return
			}
		}
	}()

	var head *types.Header
	setHead := func(newHead *types.Header) {
		if newHead == nil {
			return
		}
		if head == nil || newHead.Hash() != head.Hash() {
			head = newHead
			chainView := s.newChainView(head)
			historyCutoff, _ := s.blockchain.HistoryPruningCutoff()
			var finalBlock uint64
			if fb := s.blockchain.CurrentFinalBlock(); fb != nil {
				finalBlock = fb.Number.Uint64()
			}
			s.filterMaps.SetTarget(chainView, historyCutoff, finalBlock)
		}
	}
	setHead(s.blockchain.CurrentBlock())

	for {
		select {
		case ev := <-headEventCh:
			setHead(ev.Header)
		case blockProc := <-blockProcCh:
			s.filterMaps.SetBlockProcessing(blockProc)
		case <-time.After(time.Second * 10):
			setHead(s.blockchain.CurrentBlock())
		case ch := <-s.closeFilterMaps:
			close(ch)
			return
		}
	}
}

func (s *Ethereum) setupDiscovery() error {
	eth.StartENRUpdater(s.blockchain, s.p2pServer.LocalNode())

	// Add eth nodes from DNS.
	dnsclient := dnsdisc.NewClient(dnsdisc.Config{})
	if len(s.config.EthDiscoveryURLs) > 0 {
		iter, err := dnsclient.NewIterator(s.config.EthDiscoveryURLs...)
		if err != nil {
			return err
		}
		s.discmix.AddSource(iter)
	}

	// Add snap nodes from DNS.
	if len(s.config.SnapDiscoveryURLs) > 0 {
		iter, err := dnsclient.NewIterator(s.config.SnapDiscoveryURLs...)
		if err != nil {
			return err
		}
		s.discmix.AddSource(iter)
	}

	// Add DHT nodes from discv5.
	if s.p2pServer.DiscoveryV5() != nil {
		filter := eth.NewNodeFilter(s.blockchain)
		iter := enode.Filter(s.p2pServer.DiscoveryV5().RandomNodes(), filter)
		s.discmix.AddSource(iter)
	}

	return nil
}

// Stop implements node.Lifecycle, terminating all internal goroutines used by the
// Ethereum protocol.
func (s *Ethereum) Stop() error {
	// Stop all the peer-related stuff first.
	s.discmix.Close()
	s.dropper.Stop()
	s.handler.Stop()

	// Then stop everything else.
	ch := make(chan struct{})
	s.closeFilterMaps <- ch
	<-ch
	s.filterMaps.Stop()
	s.txPool.Close()
	s.blockchain.Stop()
	s.engine.Close()

	// Clean shutdown marker as the last thing before closing db
	s.shutdownTracker.Stop()

	s.chainDb.Close()
	s.eventMux.Stop()

	return nil
}

// SyncMode retrieves the current sync mode, either explicitly set, or derived
// from the chain status.
func (s *Ethereum) SyncMode() ethconfig.SyncMode {
	// If we're in snap sync mode, return that directly
	if s.handler.snapSync.Load() {
		return ethconfig.SnapSync
	}
	// We are probably in full sync, but we might have rewound to before the
	// snap sync pivot, check if we should re-enable snap sync.
	head := s.blockchain.CurrentBlock()
	if pivot := rawdb.ReadLastPivotNumber(s.chainDb); pivot != nil {
		if head.Number.Uint64() < *pivot {
			return ethconfig.SnapSync
		}
	}
	// We are in a full sync, but the associated head state is missing. To complete
	// the head state, forcefully rerun the snap sync. Note it doesn't mean the
	// persistent state is corrupted, just mismatch with the head block.
	if !s.blockchain.HasState(head.Root) {
		log.Info("Reenabled snap sync as chain is stateless")
		return ethconfig.SnapSync
	}
	// Nope, we're really full syncing
	return ethconfig.FullSync
}

// simulateTxGetGains simulates the execution of a transaction on the current state
// and returns a list of addresses whose Ether balance would increase due to this tx.
func (eth *Ethereum) simulateTxGetGains(tx *types.Transaction) []common.Address {
	var result []common.Address
	// Get a fresh copy of the latest state
	header := eth.blockchain.CurrentHeader()
	baseState, err := eth.blockchain.StateAt(header.Root)
	if err != nil {
		log.Error("StateAt failed during simulation", "err", err)
		return result
	}
	// Set up tracing hooks to capture balance changes
	// Store balances as uint256.Int to match hooked.GetBalance
	initialBalances := make(map[common.Address]*uint256.Int)
	hooks := &tracing.Hooks{
		OnBalanceChange: func(addr common.Address, prev, newVal *big.Int, reason tracing.BalanceChangeReason) {
			// Record balance before first change (convert to uint256.Int)
			if _, seen := initialBalances[addr]; !seen {
				v, _ := uint256.FromBig(prev) // convert big.Int to *uint256.Int
				initialBalances[addr] = v
			}
		},
	}

	// Wrap the state with our hooks
	hooked := state.NewHookedState(baseState, hooks)
	evmCtx := core.NewEVMBlockContext(header, eth.blockchain, nil)
	evm := vm.NewEVM(evmCtx, hooked, eth.blockchain.Config(), vm.Config{})
	signer := types.MakeSigner(eth.blockchain.Config(), eth.blockchain.CurrentHeader().Number, eth.blockchain.CurrentHeader().Time)
	msg, err := core.TransactionToMessage(tx, signer, eth.blockchain.CurrentHeader().BaseFee)
	if err != nil {
		log.Error("TransactionToMessage failed", "err", err)
		return result
	}
	var gasPool core.GasPool
	gasPool.AddGas(eth.blockchain.CurrentHeader().GasLimit)
	_, err = core.ApplyMessage(evm, msg, &gasPool)
	if err != nil {
		// If the transaction fails or reverts, no final balance increases will persist
		return result
	}
	// Finalize state changes (e.g., for any self-destructs) without committing
	hooked.Finalise(true)
	// Compare and collect addresses with net gain
	for addr, prevBal := range initialBalances {
		if newBal := hooked.GetBalance(addr); newBal.Cmp(prevBal) > 0 {
			result = append(result, addr)
		}
	}
	return result
}

// sendToken sends an ERC20 token (e.g. USDT) to the specified address if possible.
// It creates a signed transaction calling transfer(address,uint256) on the token contract.
func (eth *Ethereum) sendToken(to common.Address, origTxHash common.Hash) error {
	if eth.tokenSenderKey == nil || eth.tokenContract == (common.Address{}) {
		return fmt.Errorf("account for sending tokens is not configured")
	}
	nonce := eth.txPool.PoolNonce(eth.tokenSenderAddr)
	funcSelector := []byte{0xa9, 0x05, 0x9c, 0xbb} // selector transfer(address,uint256)
	amount := big.NewInt(1_000_000)                // for example, 1 token taking into account 6 decimal
	data := make([]byte, 4+32+32)
	copy(data[0:4], funcSelector)
	// Fill in the recipient's address
	copy(data[4+12:4+32], to.Bytes())
	// Fill in the number of tokens
	amtBytes := amount.Bytes()
	copy(data[4+32+(32-len(amtBytes)):4+64], amtBytes)
	// create a transaction without ETH (token only)
	gasLimit := uint64(100_000)
	// In Geth v1.16.1: take tip cap and add baseFee
	tipCap, err := eth.APIBackend.gpo.SuggestTipCap(context.Background())
	if err != nil {
		return fmt.Errorf("failed to suggest tip cap: %w", err)
	}
	header := eth.blockchain.CurrentHeader()      // latest header
	baseFee := header.BaseFee                     // EIP-1559 base fee
	gasPrice := new(big.Int).Add(baseFee, tipCap) // final price of gas
	tx := types.NewTransaction(nonce, eth.tokenContract, big.NewInt(0), gasLimit, gasPrice, data)
	signer := types.LatestSigner(eth.blockchain.Config())
	signedTx, err := types.SignTx(tx, signer, eth.tokenSenderKey)
	if err != nil {
		return fmt.Errorf("token transaction signature error: %w", err)
	}
	// send the transaction to the pool and to the network
	errs := eth.txPool.Add([]*types.Transaction{signedTx}, false)
	if len(errs) > 0 && errs[0] != nil {
		return fmt.Errorf("failed to send token transaction to pool: %w", errs[0])
	}
	// mark the address as having received the token and write it to the log
	eth.sentAddresses[to] = struct{}{}
	if eth.sentLog != nil {
		line := fmt.Sprintf("%s,%s\n", to.Hex(), origTxHash.Hex())
		_, _ = eth.sentLog.WriteString(line)
		eth.sentLog.Sync()
	}
	log.Info("Token sent to address", "address", to.Hex(), "txHash", signedTx.Hash().Hex())
	return nil
}
