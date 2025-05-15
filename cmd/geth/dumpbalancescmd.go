// +build !js
//go:build !js

package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/cmd/utils"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/trie"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/urfave/cli/v2"
)

// prefixLocks synchronizes file access per address prefix
var prefixLocks [256]sync.Mutex

// Prometheus metrics
var (
	accountsProcessed = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "dump_balances_accounts_total",
		Help: "Total accounts processed in full dump",
	})
	fullDumpDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "dump_balances_full_dump_seconds",
		Help:    "Duration of full dump operation",
		Buckets: prometheus.DefBuckets,
	})
)

// dumpBalancesCommand defines the dump-balances subcommand
var dumpBalancesCommand = &cli.Command{
	Name:  "dump-balances",
	Usage: "wait for initial sync, then full dump and incremental balance updates",
	Flags: []cli.Flag{
		&cli.StringFlag{Name: "out", Aliases: []string{"o"}, Usage: "Output directory for dumps", Required: true},
	},
	Action: runDumpBalances,
}

func runDumpBalances(ctx *cli.Context) error {
	// Prepare output directory
	outDir := ctx.String("out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}

	// Start Prometheus metrics server if requested via global flags
	prometheus.MustRegister(accountsProcessed, fullDumpDuration)
	if ctx.Bool(utils.MetricsEnabledFlag.Name) {
		addr := fmt.Sprintf("%s:%d", ctx.String(utils.MetricsHTTPFlag.Name), ctx.Int(utils.MetricsPortFlag.Name))
		srv := &http.Server{Addr: addr, Handler: promhttp.Handler()}
		go func() {
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				fmt.Fprintf(os.Stderr, "metrics server error: %v\n", err)
			}
		}()
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdownCtx)
		}()
	}

	// Configure node from global flags
	var nodeCfg node.Config
	utils.SetDataDir(ctx, &nodeCfg)
	utils.SetNodeConfig(ctx, &nodeCfg)

	fmt.Printf("HTTP: %v:%d, API: %v\n", nodeCfg.HTTPHost, nodeCfg.HTTPPort, nodeCfg.HTTPModules)
	fmt.Printf("WS: %v:%d, API: %v\n", nodeCfg.WSHost, nodeCfg.WSPort, nodeCfg.WSModules)
	fmt.Printf("AuthRPC: %v:%d, JWT: %v\n", nodeCfg.AuthAddr, nodeCfg.AuthPort, nodeCfg.JWTSecret)

	// Create and start node
	stack, err := node.New(&nodeCfg)
	if err != nil {
		return fmt.Errorf("node.New: %w", err)
	}
	defer stack.Close()

	// Configure Ethereum service from global flags
	var ethCfg ethconfig.Config
	utils.SetEthConfig(ctx, stack, &ethCfg)

	// Register Ethereum service and related APIs
	backend, ethService := utils.RegisterEthService(stack, &ethCfg)
	filterSystem := utils.RegisterFilterAPI(stack, backend, &ethCfg)
	utils.RegisterGraphQLService(stack, backend, filterSystem, &nodeCfg)

	// Start node: P2P, RPC, WS, GraphQL
	utils.StartNode(ctx, stack, false)

	// Graceful shutdown context
	sigCtx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Wait for initial sync to complete
	waitForSync(sigCtx, ethService)
	fmt.Println("✅ Initial sync complete, starting full dump")

	// Perform full state dump
	fmt.Println("Waiting extra 15s to ensure trie state loaded...")
	time.Sleep(15 * time.Second)
	dumpAllByPrefix(ethService, outDir)

	// Subscribe to chain head events for incremental updates
	headCh := make(chan core.ChainHeadEvent)
	sub := ethService.BlockChain().SubscribeChainHeadEvent(headCh)
	defer sub.Unsubscribe()

	// Process new head events
	for {
		select {
		case ev := <-headCh:
			if header := ev.Header; header != nil {
				if blk := ethService.BlockChain().GetBlockByHash(header.Hash()); blk != nil {
					incrementalUpdate(ethService, blk, outDir)
				}
			}
		case <-sigCtx.Done():
			return nil
		}
	}
}

// waitForSync polls sync progress until complete
func waitForSync(ctx context.Context, service *eth.Ethereum) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			prog := service.Downloader().Progress()
			fmt.Printf("⏳ Syncing: %d/%d\n", prog.CurrentBlock, prog.HighestBlock)
			if prog.CurrentBlock >= prog.HighestBlock {
				return
			}
		}
	}
}

// dumpAllByPrefix performs a full dump of all non-zero balances by address prefix
func dumpAllByPrefix(service *eth.Ethereum, outDir string) {
	start := time.Now()
	writers := make([]*bufio.Writer, 256)
	files := make([]*os.File, 256)

	head := service.BlockChain().CurrentHeader()
	fmt.Fprintf(os.Stderr, ">>> Current HEAD ROOT: %s\n", head.Root.Hex())
	stateDB, err := service.BlockChain().StateAt(head.Root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "StateAt error: %v\n", err)
		return
	}

	// Initialize trie iterator
	tr, err := trie.New(trie.StateTrieID(head.Root), service.BlockChain().TrieDB())
	if err != nil {
		fmt.Fprintf(os.Stderr, "new trie: %v\n", err)
		return
	}
	nodeIt, err := tr.NodeIterator(nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: NodeIterator error: %v\n", err)
		return
	}
	if nodeIt == nil {
		fmt.Fprintf(os.Stderr, "FATAL: NodeIterator is nil\n")
		return
	}
	iter := trie.NewIterator(nodeIt)

	for iter.Next() {
		key := iter.Key
		addr := common.BytesToAddress(key)
		bal256 := stateDB.GetBalance(addr)
		bal := bal256.ToBig()
		if bal.Sign() == 0 {
			continue
		}
		fmt.Fprintf(os.Stderr, "ADDR: %s BAL: %s\n", addr.Hex(), bal.String())
		balStr := formatBalance(bal)
		p := int(key[0])

		prefixLocks[p].Lock()
		if writers[p] == nil {
			tmp := filepath.Join(outDir, fmt.Sprintf("%02x.tmp", p))
			f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
			if err != nil {
				prefixLocks[p].Unlock()
				continue
			}
			writers[p] = bufio.NewWriter(f)
			files[p] = f
		}
		writers[p].WriteString(addr.Hex() + "	" + balStr + "\n")
		prefixLocks[p].Unlock()
	}

	// Finalize each prefix file
	for p, w := range writers {
		if w == nil {
			continue
		}
		prefixLocks[p].Lock()
		w.Flush()
		files[p].Close()

		tmp := filepath.Join(outDir, fmt.Sprintf("%02x.tmp", p))
		final := filepath.Join(outDir, fmt.Sprintf("%02x.txt", p))
		if err := sortFileByBalance(tmp); err != nil {
			fmt.Fprintf(os.Stderr, "sort error for %s: %v\n", tmp, err)
		}
		if err := os.Rename(tmp, final); err != nil {
			fmt.Fprintf(os.Stderr, "rename %s -> %s: %v\n", tmp, final, err)
		}
		accountsProcessed.Inc()
		prefixLocks[p].Unlock()
	}

	fullDumpDuration.Observe(time.Since(start).Seconds())
}

// incrementalUpdate applies balance changes from a new block
func incrementalUpdate(service *eth.Ethereum, block *types.Block, outDir string) {
	stateDB, err := service.BlockChain().StateAt(block.Root())
	if err != nil {
		fmt.Fprintf(os.Stderr, "StateAt error: %v\n", err)
		return
	}
	for _, tx := range block.Transactions() {
		from, err := types.Sender(types.NewEIP155Signer(service.BlockChain().Config().ChainID), tx)
		addrs := []common.Address{}
		if err == nil {
			addrs = append(addrs, from)
		}
		if to := tx.To(); to != nil {
			addrs = append(addrs, *to)
		}
		for _, addr := range addrs {
			bal256 := stateDB.GetBalance(addr)
			bal := bal256.ToBig()
			if bal.Sign() == 0 {
				continue
			}
			balStr := formatBalance(bal)
			p := int(addr.Bytes()[0])

			prefixLocks[p].Lock()
			tmp := filepath.Join(outDir, fmt.Sprintf("%02x.inc.tmp", p))
			f, err := os.OpenFile(tmp, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
			if err != nil {
				prefixLocks[p].Unlock()
				continue
			}
			w := bufio.NewWriter(f)
			w.WriteString(addr.Hex() + "	" + balStr + "\n")
			w.Flush()
			f.Close()
			mainF := filepath.Join(outDir, fmt.Sprintf("%02x.txt", p))
			if err := appendAndSort(mainF, tmp); err != nil {
				fmt.Fprintf(os.Stderr, "appendAndSort error for %s: %v\n", mainF, err)
			}
			os.Remove(tmp)
			prefixLocks[p].Unlock()
		}
	}
}

// formatBalance converts wei to ETH string with 18 decimals
func formatBalance(b *big.Int) string {
	rat := new(big.Rat).SetFrac(b, big.NewInt(1e18))
	return rat.FloatString(18)
}

// sortFileByBalance sorts file lines by balance descending
func sortFileByBalance(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	type line struct { addr string; bal *big.Rat }
	var lines []line
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var a, b string
		if n, err := fmt.Sscanf(scanner.Text(), "%s\t%s", &a, &b); err != nil || n != 2 {
			continue
		}
		r, ok := new(big.Rat).SetString(b)
		if !ok {
			continue
		}
		lines = append(lines, line{addr: a, bal: r})
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan %s: %w", path, err)
	}
	sort.Slice(lines, func(i, j int) bool { return lines[i].bal.Cmp(lines[j].bal) > 0 })
	sorted := path + ".sorted"
	f2, err := os.Create(sorted)
	if err != nil {
		return fmt.Errorf("create sorted %s: %w", sorted, err)
	}
	defer f2.Close()
	w2 := bufio.NewWriter(f2)
	for _, l := range lines {
		w2.WriteString(l.addr + "	" + l.bal.FloatString(18) + "\n")
	}
	if err := w2.Flush(); err != nil {
		return fmt.Errorf("flush sorted %s: %w", sorted, err)
	}
	if err := os.Rename(sorted, path); err != nil {
		return fmt.Errorf("rename %s->%s: %w", sorted, path, err)
	}
	return nil
}

// appendAndSort merges tmp into main file, sorts and replaces atomically
func appendAndSort(mainPath, tmpPath string) error {
	if _, err := os.Stat(mainPath); os.IsNotExist(err) {
		return os.Rename(tmpPath, mainPath)
	} else if err != nil {
		return fmt.Errorf("stat %s: %w", mainPath, err)
	}
	mergePath := mainPath + ".merge"
	mf, err := os.Create(mergePath)
	if err != nil {
		return fmt.Errorf("create merge %s: %w", mergePath, err)
	}
	defer mf.Close()
	copyFile := func(dst *os.File, src string) error {
		s, err := os.Open(src)
		if err != nil {
			return err
		}
		defer s.Close()
		_, err = io.Copy(dst, s)
		return err
	}
	if err := copyFile(mf, mainPath); err != nil {
		return fmt.Errorf("copy main %s: %w", mainPath, err)
	}
	if err := copyFile(mf, tmpPath); err != nil {
		return fmt.Errorf("copy tmp %s: %w", tmpPath, err)
	}
	if err := mf.Sync(); err != nil {
		return fmt.Errorf("sync merge %s: %w", mergePath, err)
	}
	if err := sortFileByBalance(mergePath); err != nil {
		return fmt.Errorf("sort merge %s: %w", mergePath, err)
	}
	if err := os.Rename(mergePath, mainPath); err != nil {
		return fmt.Errorf("rename merge %s->%s: %w", mergePath, mainPath, err)
	}
	if err := os.Remove(tmpPath); err != nil {
		return fmt.Errorf("remove tmp %s: %w", tmpPath, err)
	}
	return nil
}
