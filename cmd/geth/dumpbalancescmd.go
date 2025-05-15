// +build !js
//go:build !js

package main

import (
    "bufio"
    "context"
    "encoding/json"
    "fmt"
    "strings"
    "io"
    "math/big"
    "net/http"
    "os"
    "os/signal"
    "path/filepath"
    "runtime"
    "sort"
    "sync"
    "syscall"
    "time"
    "log"

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
        &cli.BoolFlag{Name: "verbose", Aliases: []string{"v"}, Usage: "Enable verbose logging"},
    },
    Action: runDumpBalances,
}

var verboseLog bool

func runDumpBalances(ctx *cli.Context) error {
    verboseLog = ctx.Bool("verbose")

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
                log.Printf("metrics server error: %v", err)
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

    // After full dump, check sync state
    head := ethService.BlockChain().CurrentHeader()
    highest := ethService.BlockChain().CurrentHeader()
    if highest != nil && head != nil {
        if highest.Number.Uint64() > head.Number.Uint64() {
            diff := highest.Number.Uint64() - head.Number.Uint64()
            if diff < 50 {
                if verboseLog {
                    log.Printf("Current block difference %d < 50 after dump, waiting for full sync", diff)
                }
                waitForSync(sigCtx, ethService)
                if verboseLog {
                    log.Printf("Full sync reached, performing additional full dump with progress")
                }
                dumpAllByPrefix(ethService, outDir)
            }
        }
    }

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
            log.Printf("Syncing: %d/%d", prog.CurrentBlock, prog.HighestBlock)
            if prog.CurrentBlock >= prog.HighestBlock {
                return
            }
        }
    }
}

// DumpProgress holds progress information for full dump
type DumpProgress struct {
    Block        uint64 `json:"block"`
    PrefixesDone []int  `json:"prefixes_done"`
}

// saveProgress writes progress to a file
func saveProgress(outDir string, progress *DumpProgress) {
    data, err := json.Marshal(progress)
    if err != nil {
        if verboseLog {
            log.Printf("Error marshaling progress: %v", err)
        }
        return
    }
    tmpPath := filepath.Join(outDir, "dump_progress.tmp")
    finalPath := filepath.Join(outDir, "dump_progress.json")
    if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
        if verboseLog {
            log.Printf("Error writing progress temp file: %v", err)
        }
        return
    }
    if err := os.Rename(tmpPath, finalPath); err != nil {
        if verboseLog {
            log.Printf("Error renaming progress file: %v", err)
        }
    }
}

// loadProgress reads progress from a file if exists
func loadProgress(outDir string) (*DumpProgress, error) {
    path := filepath.Join(outDir, "dump_progress.json")
    data, err := os.ReadFile(path)
    if err != nil {
        return nil, err
    }
    var progress DumpProgress
    if err := json.Unmarshal(data, &progress); err != nil {
        return nil, err
    }
    return &progress, nil
}

// dumpAllByPrefix performs a full dump of all non-zero balances by address prefix
func dumpAllByPrefix(service *eth.Ethereum, outDir string) {
    start := time.Now()

    // Determine number of workers (75% of CPU)
    numCPU := runtime.NumCPU()
    numWorkers := int(float64(numCPU) * 0.75)
    if numWorkers < 1 {
        numWorkers = 1
    }
    if verboseLog {
        log.Printf("Full dump using %d workers (CPU: %d)", numWorkers, numCPU)
    }

    // Load progress if exists
    progress, err := loadProgress(outDir)
    if err != nil {
        progress = &DumpProgress{PrefixesDone: []int{}}
    }
    done := make(map[int]bool)
    for _, p := range progress.PrefixesDone {
        done[p] = true
    }

    head := service.BlockChain().CurrentHeader()
    if head == nil {
        log.Printf("Failed to get current header for dump")
        return
    }
    if progress.Block == 0 {
        progress.Block = head.Number.Uint64()
    }
    if verboseLog {
        log.Printf("Dumping state at block %d", head.Number.Uint64())
    }

    // Channel to send accounts to workers
    type account struct {
        prefix int
        addr   common.Address
        bal    *big.Int
    }
    accountCh := make(chan account, 1024)

    // Assign prefixes to workers evenly
    prefixes := make([][]int, numWorkers)
    for i := 0; i < 256; i++ {
        if done[i] {
            continue
        }
        prefixes[i%numWorkers] = append(prefixes[i%numWorkers], i)
    }

    var wg sync.WaitGroup
    // Worker function
    for w := 0; w < numWorkers; w++ {
        if len(prefixes[w]) == 0 {
            continue
        }
        wg.Add(1)
        go func(workerPrefixes []int) {
            defer wg.Done()
            writers := make(map[int]*bufio.Writer)
            files := make(map[int]*os.File)
            for acc := range accountCh {
                p := acc.prefix
                // only handle prefixes assigned to this worker
                found := false
                for _, wp := range workerPrefixes {
                    if wp == p {
                        found = true
                        break
                    }
                }
                if !found {
                    continue
                }
                if _, ok := writers[p]; !ok {
                    tmpPath := filepath.Join(outDir, fmt.Sprintf("%02x.tmp", p))
                    f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
                    if err != nil {
                        if verboseLog {
                            log.Printf("Error creating tmp file %s: %v", tmpPath, err)
                        }
                        continue
                    }
                    writers[p] = bufio.NewWriter(f)
                    files[p] = f
                }
                balanceStr := formatBalance(acc.bal)
                writers[p].WriteString(acc.addr.Hex() + "\t" + balanceStr + "\n")
            }
            // Flush and sort each prefix file for this worker
            for p, w := range writers {
                w.Flush()
                files[p].Close()
                tmp := filepath.Join(outDir, fmt.Sprintf("%02x.tmp", p))
                final := filepath.Join(outDir, fmt.Sprintf("%02x.txt", p))
                if err := sortFileByBalance(tmp); err != nil {
                    log.Printf("sort error for %s: %v", tmp, err)
                }
                if err := os.Rename(tmp, final); err != nil {
                    log.Printf("rename %s -> %s: %v", tmp, final, err)
                }
                accountsProcessed.Inc()
                if verboseLog {
                    log.Printf("Finished prefix %02x", p)
                }
                // Update progress
                progress.PrefixesDone = append(progress.PrefixesDone, p)
                saveProgress(outDir, progress)
            }
        }(prefixes[w])
    }

    // Iterate state trie and send to workers
    stateDB, err := service.BlockChain().StateAt(head.Root)
    if err != nil {
        log.Printf("StateAt error: %v", err)
        return
    }
    tr, err := trie.New(trie.StateTrieID(head.Root), service.BlockChain().TrieDB())
    if err != nil {
        log.Printf("new trie: %v", err)
        return
    }
    nodeIt, err := tr.NodeIterator(nil)
    if err != nil {
        log.Printf("NodeIterator error: %v", err)
        return
    }
    if nodeIt == nil {
        log.Printf("NodeIterator is nil")
        return
    }
    iter := trie.NewIterator(nodeIt)
    if verboseLog {
        log.Printf("Starting iteration...")
    }
    var count, nonZero int
    for iter.Next() {
        count++
        key := iter.Key
        addr := common.BytesToAddress(key)
        bal := stateDB.GetBalance(addr)
        if bal == nil {
            if verboseLog {
                log.Printf("Balance is nil for addr %s", addr.Hex())
            }
            continue
        }
        balBig := bal.ToBig()
        if balBig == nil {
            if verboseLog {
                log.Printf("ToBig is nil for addr %s", addr.Hex())
            }
            continue
        }
        if balBig.Sign() == 0 {
            continue
        }
        p := int(key[0])
        // skip if this prefix was already done
        if done[p] {
            continue
        }
        nonZero++
        accountCh <- account{prefix: p, addr: addr, bal: balBig}
        if verboseLog {
            log.Printf("Account: %s Balance: %s", addr.Hex(), balBig.String())
        }
    }
    close(accountCh)
    wg.Wait()
    if verboseLog {
        log.Printf("Iteration complete. Total entries: %d, non-zero balances: %d", count, nonZero)
    }

    fullDumpDuration.Observe(time.Since(start).Seconds())
}

// removeAddresses removes lines with given addresses from the file at mainPath
func removeAddresses(mainPath string, addresses []common.Address) error {
    if len(addresses) == 0 {
        return nil
    }
    f, err := os.Open(mainPath)
    if err != nil {
        return fmt.Errorf("open %s: %w", mainPath, err)
    }
    defer f.Close()
    removeSet := make(map[string]struct{}, len(addresses))
    for _, addr := range addresses {
        removeSet[addr.Hex()] = struct{}{}
    }
    tmpPath := mainPath + ".new"
    tf, err := os.Create(tmpPath)
    if err != nil {
        return fmt.Errorf("create %s: %w", tmpPath, err)
    }
    defer tf.Close()
    scanner := bufio.NewScanner(f)
    writer := bufio.NewWriter(tf)
    for scanner.Scan() {
        line := scanner.Text()
        fields := strings.Fields(line)
        if len(fields) == 0 {
            continue
        }
        addr := fields[0]
        if _, ok := removeSet[addr]; ok {
            continue
        }
        writer.WriteString(line + "\n")
    }
    if err := scanner.Err(); err != nil {
        return fmt.Errorf("scan %s: %w", mainPath, err)
    }
    writer.Flush()
    if err := os.Rename(tmpPath, mainPath); err != nil {
        return fmt.Errorf("rename %s -> %s: %w", tmpPath, mainPath, err)
    }
    return nil
}

// incrementalUpdate applies balance changes from a new block
func incrementalUpdate(service *eth.Ethereum, block *types.Block, outDir string) {
    if verboseLog {
        log.Printf("Incremental update for block %d", block.NumberU64())
    }
    stateDB, err := service.BlockChain().StateAt(block.Root())
    if err != nil {
        log.Printf("StateAt error: %v", err)
        return
    }
    // Group updates per prefix
    updates := make(map[int]map[string]*big.Int)
    zeros := make(map[int]map[string]struct{})
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
            p := int(addr.Bytes()[0])
            addrStr := addr.Hex()
            if bal.Sign() == 0 {
                if updates[p] != nil {
                    delete(updates[p], addrStr)
                }
                if zeros[p] == nil {
                    zeros[p] = make(map[string]struct{})
                }
                zeros[p][addrStr] = struct{}{}
            } else {
                if updates[p] == nil {
                    updates[p] = make(map[string]*big.Int)
                }
                updates[p][addrStr] = bal
                if zeros[p] != nil {
                    delete(zeros[p], addrStr)
                }
            }
        }
    }
    // Apply updates per prefix
    var wgUpdate sync.WaitGroup
    for p, addrMap := range updates {
        wgUpdate.Add(1)
        go func(p int, addrMap map[string]*big.Int) {
            defer wgUpdate.Done()
            tmp := filepath.Join(outDir, fmt.Sprintf("%02x.inc.tmp", p))
            f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
            if err != nil {
                log.Printf("Error creating inc tmp for prefix %02x: %v", p, err)
                return
            }
            w := bufio.NewWriter(f)
            for addrStr, bal := range addrMap {
                balStr := formatBalance(bal)
                w.WriteString(addrStr + "\t" + balStr + "\n")
            }
            w.Flush()
            f.Close()
            mainF := filepath.Join(outDir, fmt.Sprintf("%02x.txt", p))
            if err := appendAndSort(mainF, tmp); err != nil {
                log.Printf("appendAndSort error for %s: %v", mainF, err)
            }
            os.Remove(tmp)
            if verboseLog {
                log.Printf("Updated prefix %02x with %d entries", p, len(addrMap))
            }
        }(p, addrMap)
    }
    wgUpdate.Wait()
    // Apply removals per prefix
    var wgRemove sync.WaitGroup
    for p, zeroMap := range zeros {
        if len(zeroMap) == 0 {
            continue
        }
        wgRemove.Add(1)
        go func(p int, zeroMap map[string]struct{}) {
            defer wgRemove.Done()
            addrs := []common.Address{}
            for addrStr := range zeroMap {
                addr := common.HexToAddress(addrStr)
                addrs = append(addrs, addr)
            }
            mainF := filepath.Join(outDir, fmt.Sprintf("%02x.txt", p))
            if err := removeAddresses(mainF, addrs); err != nil {
                log.Printf("removeAddresses error for %s: %v", mainF, err)
            }
            if verboseLog {
                log.Printf("Removed %d addresses from prefix %02x", len(addrs), p)
            }
        }(p, zeroMap)
    }
    wgRemove.Wait()
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
    type line struct{ addr string; bal *big.Rat }
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
        w2.WriteString(l.addr + "\t" + l.bal.FloatString(18) + "\n")
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
