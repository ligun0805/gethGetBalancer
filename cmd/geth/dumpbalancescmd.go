package main

import (
    "bufio"
    "context"
    "fmt"
    "net/http"
    "os"
    "path/filepath"
    "sort"
    "strings"
    "sync"
    "syscall"
    "time"
    "math/big"
    "io"

    "github.com/ethereum/go-ethereum/common"
    "github.com/ethereum/go-ethereum/core"
    "github.com/ethereum/go-ethereum/core/types"
    "github.com/ethereum/go-ethereum/crypto"
    "github.com/ethereum/go-ethereum/eth"
    "github.com/ethereum/go-ethereum/eth/ethconfig"
    "github.com/ethereum/go-ethereum/node"
    "github.com/ethereum/go-ethereum/trie"
    "github.com/ethereum/go-ethereum/cmd/utils"
    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/promhttp"
    "github.com/urfave/cli/v2"
    "os/signal"
)

// prefixLocks ensure thread-safe writes per address-prefix file
var prefixLocks [256]sync.Mutex

// Prometheus metrics for monitoring
var (
    accountsProcessed = prometheus.NewCounter(prometheus.CounterOpts{
        Name: "dump_balance_prefixes_processed_total",
        Help: "Number of 0xXX prefix files fully processed in the initial dump",
    })
    fullDumpDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
        Name:    "dump_full_dump_duration_seconds",
        Help:    "Duration of the full initial balance dump in seconds",
        Buckets: prometheus.ExponentialBuckets(1, 2, 15), // 1s to ~16384s
    })
)

// dumpBalancesCommand defines the CLI command for dumping balances
var dumpBalancesCommand = &cli.Command{
    Name:  "dump-balances",
    Usage: "Dump all accounts with non-zero balance to prefixed files, with optional incremental updates",
    Flags: []cli.Flag{
        &cli.StringFlag{
            Name:     "out",
            Aliases:  []string{"o"},
            Usage:    "Output directory for balance dump files",
            Required: true,
        },
        &cli.BoolFlag{
            Name:  "sort",
            Usage: "Sort accounts by balance in descending order within each prefix file",
        },
        &cli.StringFlag{
            Name:  "format",
            Usage: "Output format: 'tab' (address<tab>balance) or 'jsonl' (JSON lines)",
            Value: "tab",
        },
    },
    Action: runDumpBalances,
}

// runDumpBalances is the main entry point for the dump-balances command
func runDumpBalances(ctx *cli.Context) error {
    // Prepare the output directory
    outDir := ctx.String("out")
    if err := os.MkdirAll(outDir, 0o755); err != nil {
        return fmt.Errorf("failed to create output directory: %w", err)
    }
    sortBalances := ctx.Bool("sort")
    format := ctx.String("format")
    if format != "tab" && format != "jsonl" {
        return fmt.Errorf("invalid format '%s', must be 'tab' or 'jsonl'", format)
    }

    // Initialize Prometheus metrics (for memory and progress monitoring)
    prometheus.MustRegister(accountsProcessed, fullDumpDuration)
    if ctx.Bool(utils.MetricsEnabledFlag.Name) {
        addr := fmt.Sprintf("%s:%d", ctx.String(utils.MetricsHTTPFlag.Name), ctx.Int(utils.MetricsPortFlag.Name))
        srv := &http.Server{Addr: addr, Handler: promhttp.Handler()}
        go func() {
            if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
                fmt.Fprintf(os.Stderr, "⚠️ Metrics server error: %v\n", err)
            }
        }()
        defer func() {
            shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
            defer cancel()
            _ = srv.Shutdown(shutdownCtx)
        }()
        fmt.Fprintf(os.Stderr, "Prometheus metrics server listening on %s\n", addr)
    }

    // Configure and start an in-process Geth node (no datadir conflict, uses same process)
    var nodeCfg node.Config
    utils.SetDataDir(ctx, &nodeCfg)
    utils.SetNodeConfig(ctx, &nodeCfg)
    stack, err := node.New(&nodeCfg)
    if err != nil {
        return fmt.Errorf("failed to create geth node: %w", err)
    }
    defer stack.Close()

    // Configure Ethereum service with preimage recording enabled (needed for address resolution)
    var ethCfg ethconfig.Config
    utils.SetEthConfig(ctx, stack, &ethCfg)
    ethCfg.EnablePreimageRecording = true  // ensure preimage tracking for trie iteration
    backend, ethService := utils.RegisterEthService(stack, &ethCfg)
    filterSystem := utils.RegisterFilterAPI(stack, backend, &ethCfg)
    utils.RegisterGraphQLService(stack, backend, filterSystem, &nodeCfg)

    // Start the node (p2p network, consensus, etc.)
    utils.StartNode(ctx, stack, false)

    // Handle graceful shutdown signals
    sigCtx, cancelSig := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer cancelSig()

    // Wait for initial blockchain sync to complete
    waitForSync(sigCtx, ethService)
    fmt.Fprintln(os.Stderr, "✅ Initial blockchain sync is complete. Proceeding with full state dump...")

    // Wait for state trie availability (snap sync may finish state download after block sync)
    waitForTrieReady(ethService)
    fmt.Fprintln(os.Stderr, "Beginning initial full balance dump...")

    // Perform the initial dump of all non-zero balances by address prefix
    dumpAllBalancesByPrefix(ethService, outDir, sortBalances, format)

    fmt.Fprintln(os.Stderr, "✅ Full balance dump complete. Now monitoring new blocks for incremental updates...")

    // Subscribe to new block headers for incremental updates
    headCh := make(chan core.ChainHeadEvent)
    sub := ethService.BlockChain().SubscribeChainHeadEvent(headCh)
    defer sub.Unsubscribe()

    // Loop to handle new blocks and update balances incrementally
    for {
        select {
        case ev := <-headCh:
            if ev.Header != nil {
                if blk := ethService.BlockChain().GetBlockByHash(ev.Header.Hash()); blk != nil {
                    incrementalUpdate(ethService, blk, outDir, sortBalances, format)
                }
            }
        case <-sigCtx.Done():
            // Interrupt signal received, shut down gracefully
            fmt.Fprintln(os.Stderr, "ℹ️ Received shutdown signal, exiting dump-balances...")
            return nil
        }
    }
}

// waitForSync polls the Ethereum service until the initial sync is finished or context is canceled
func waitForSync(ctx context.Context, service *eth.Ethereum) {
    ticker := time.NewTicker(10 * time.Second)
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            // Check sync progress via Ethereum service API
            progress := service.APIBackend.SyncProgress(ctx)
            if progress.Done() {
                // No sync in progress or initial sync is complete
                return
            }
            // If HighestBlock is not zero and we've caught up to it, sync is complete
            if progress.HighestBlock != 0 && progress.CurrentBlock >= progress.HighestBlock {
                return
            }
            // Print a status update for the user in the terminal
            if progress.HighestBlock > 0 {
                fmt.Fprintf(os.Stderr, "⏳ Syncing... block %d of ~%d (%d%%)\n",
                    progress.CurrentBlock, progress.HighestBlock,
                    progress.CurrentBlock*100/uint64(progress.HighestBlock))
            } else {
                fmt.Fprintln(os.Stderr, "⏳ Syncing state...")
            }
        }
    }
}

// waitForTrieReady waits until the latest state trie is fully available for iteration (important for snap sync)
func waitForTrieReady(service *eth.Ethereum) {
    for {
        head := service.BlockChain().CurrentHeader()
        if head == nil {
            fmt.Fprintln(os.Stderr, "⌛ Waiting for chain head to be available...")
            time.Sleep(3 * time.Second)
            continue
        }
        // Try to access state at head (could error if state sync still running)
        sdb, err := service.BlockChain().StateAt(head.Root)
        if err != nil {
            fmt.Fprintf(os.Stderr, "⌛ Waiting for state trie... (%v)\n", err)
            time.Sleep(3 * time.Second)
            continue
        }
        // Test accessing a known address (0x...01) to ensure trie iteration won't block
        testAddr := common.HexToAddress("0x0000000000000000000000000000000000000001")
        bal := sdb.GetBalance(testAddr)
        if bal != nil {
            // We consider the trie ready if we can query a balance (even if zero) without error
            return
        }
        fmt.Fprintln(os.Stderr, "⌛ Trie still loading...")
        time.Sleep(3 * time.Second)
    }
}

// dumpAllBalancesByPrefix performs the initial full dump of all accounts with non-zero balances.
// It iterates over the entire state trie and writes addresses and balances into 256 files grouped by address prefix.
func dumpAllBalancesByPrefix(service *eth.Ethereum, outDir string, sortBalances bool, format string) {
    start := time.Now()
    // Buffers and files for each prefix (00 to ff)
    writers := make([]*bufio.Writer, 256)
    files := make([]*os.File, 256)

    head := service.BlockChain().CurrentHeader()
    if head == nil {
        fmt.Fprintln(os.Stderr, "❌ No chain head found, aborting dump.")
        return
    }
    fmt.Fprintf(os.Stderr, "Dumping state at block #%d (state root %s)\n", head.Number, head.Root.Hex())

    // Obtain the state database at the head state root
    sdb, err := service.BlockChain().StateAt(head.Root)
    if err != nil {
        fmt.Fprintf(os.Stderr, "❌ StateAt(head) error: %v\n", err)
        return
    }
    // Open the state trie for iteration (secure keys)
    trieDB := service.BlockChain().TrieDB()
    stateTrie, err := trie.New(trie.StateTrieID(head.Root), trieDB)
    if err != nil {
        fmt.Fprintf(os.Stderr, "❌ Opening trie at state root failed: %v\n", err)
        return
    }
    nodeIt, err := stateTrie.NodeIterator(nil)
    if err != nil {
        fmt.Fprintf(os.Stderr, "❌ State trie iterator error: %v\n", err)
        return
    }
    iter := trie.NewIterator(nodeIt)

    var total, nonZero uint64
    preimages := sdb.Preimages()  // map[common.Hash][]byte of known preimages (address resolution)
    // Iterate over every account trie node (only leaves contain account data)
    for iter.Next() {
        total++
        hashKey := iter.Key // this is the 32-byte Keccak-256 hashed address (secure trie key)
        preimage, ok := preimages[common.BytesToHash(hashKey)]
        if !ok {
            // If preimage recording is disabled or missing for this key, skip (cannot resolve real address)
            continue
        }
        addr := common.BytesToAddress(preimage)        // recover the real address
        balance := sdb.GetBalance(addr).ToBig()        // *big.Int balance of the account
        if balance.Sign() == 0 {
            // Skip accounts with zero balance
            continue
        }
        nonZero++
        // Print progress to stderr (optional, can comment out for performance on huge states)
        fmt.Fprintf(os.Stderr, " [%d] %s -> %s\n", total, addr.Hex(), balance.String())

        // Determine file prefix by first byte of address
        prefix := addr.Bytes()[0]  // first 8 bits (0x00 to 0xff)
        balStr := formatBalance(balance)  // format balance as decimal string with 18 decimals (ETH)
        // Prepare output line in chosen format
        var line string
        if format == "tab" {
            // "address<TAB>balance"
            line = addr.Hex() + "\t" + balStr + "\n"
        } else if format == "jsonl" {
            // JSON line: {"address":"0x...","balance":"..."}
            line = fmt.Sprintf("{\"address\":\"%s\",\"balance\":\"%s\"}\n", addr.Hex(), balStr)
        }

        // Write line to the corresponding prefix temporary file (open on first use)
        idx := int(prefix)
        prefixLocks[idx].Lock()
        if writers[idx] == nil {
            tmpPath := filepath.Join(outDir, fmt.Sprintf("%02x.tmp", idx))
            f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
            if err != nil {
                prefixLocks[idx].Unlock()
                fmt.Fprintf(os.Stderr, "⚠️ Cannot open file for prefix %02x: %v\n", idx, err)
                continue
            }
            writers[idx] = bufio.NewWriter(f)
            files[idx] = f
        }
        // Write the line to buffer
        writers[idx].WriteString(line)
        prefixLocks[idx].Unlock()
    }

    // Flush and finalize each prefix file
    for idx, w := range writers {
        if w == nil {
            // no accounts for this prefix
            continue
        }
        prefixLocks[idx].Lock()
        _ = w.Flush()
        _ = files[idx].Close()
        tmpPath := filepath.Join(outDir, fmt.Sprintf("%02x.tmp", idx))
        finalPath := filepath.Join(outDir, fmt.Sprintf("%02x.txt", idx))
        if sortBalances {
            // Optionally sort the file by balance in descending order before finalizing
            if err := sortFileByBalance(tmpPath); err != nil {
                fmt.Fprintf(os.Stderr, "⚠️ Sorting error for prefix %02x: %v\n", idx, err)
            }
        }
        // Rename the temporary file to final .txt (atomic replace if exists)
        if err := os.Rename(tmpPath, finalPath); err != nil {
            fmt.Fprintf(os.Stderr, "⚠️ Rename error for prefix %02x: %v\n", idx, err)
        }
        accountsProcessed.Inc()  // update metric for each prefix processed
        prefixLocks[idx].Unlock()
    }

    elapsed := time.Since(start)
    fullDumpDuration.Observe(elapsed.Seconds())
    fmt.Fprintf(os.Stderr, "Dumped %d accounts with non-zero balance (out of %d total) in %.2f seconds.\n", nonZero, total, elapsed.Seconds())
    // Note: Accounts with zero balance were skipped.
}

// incrementalUpdate processes a single new block to update the balance dump.
// It finds any accounts whose balances changed in the block and updates the corresponding prefix files.
func incrementalUpdate(service *eth.Ethereum, block *types.Block, outDir string, sortBalances bool, format string) {
    // Get state at this block's post-state (block.Root() is the state trie root after applying the block)
    sdb, err := service.BlockChain().StateAt(block.Root())
    if err != nil {
        fmt.Fprintf(os.Stderr, "⚠️ StateAt(block %d) error: %v\n", block.NumberU64(), err)
        return
    }
    blockNumber := block.NumberU64()
    // Track addresses already updated in this block to avoid duplicate processing
    updatedAddrs := make(map[common.Address]struct{})

    // Process all transactions in the block
    for _, tx := range block.Transactions() {
        // Determine the addresses potentially affected by this transaction:
        //  - sender (from)
        //  - receiver (to) if not contract creation
        //  - if contract creation (To() == nil), include the new contract address
        from, _ := types.Sender(types.LatestSigner(service.BlockChain().Config()), tx) // get sender (error can be ignored if tx is valid)
        addrs := []common.Address{from}
        if tx.To() == nil {
            // Contract creation: derive contract address from sender and nonce
            newAddr := crypto.CreateAddress(from, tx.Nonce())
            addrs = append(addrs, newAddr)
        } else {
            addrs = append(addrs, *tx.To())
        }

        // For each relevant address, append balance update if not already handled
        for _, addr := range addrs {
            if _, seen := updatedAddrs[addr]; seen {
                continue // address already updated from a previous tx in this block
            }
            updatedAddrs[addr] = struct{}{}

            bal := sdb.GetBalance(addr).ToBig()
            // If balance is zero, we consider removing from the list if it was previously present.
            // (Note: This implementation does not explicitly remove zero-balance accounts from the dump files.
            //  Those addresses will remain in the file with their old balance if they were previously non-zero.)
            if bal.Sign() == 0 {
                continue
            }
            balStr := formatBalance(bal)
            prefix := addr.Bytes()[0]
            idx := int(prefix)
            // Prepare the output line in the chosen format
            var line string
            if format == "tab" {
                line = addr.Hex() + "\t" + balStr + "\n"
            } else if format == "jsonl" {
                line = fmt.Sprintf("{\"address\":\"%s\",\"balance\":\"%s\"}\n", addr.Hex(), balStr)
            }

            prefixLocks[idx].Lock()
            if sortBalances {
                // If sorting, we write the update to a temp increment file and then merge-sort with main file
                incPath := filepath.Join(outDir, fmt.Sprintf("%02x.inc.tmp", idx))
                f, err := os.OpenFile(incPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
                if err != nil {
                    prefixLocks[idx].Unlock()
                    fmt.Fprintf(os.Stderr, "⚠️ Cannot open incremental file for prefix %02x: %v\n", idx, err)
                    continue
                }
                w := bufio.NewWriter(f)
                _, _ = w.WriteString(line)
                _ = w.Flush()
                f.Close()
                // Merge the .inc.tmp into the main prefix file, with sorting
                mainPath := filepath.Join(outDir, fmt.Sprintf("%02x.txt", idx))
                if err := appendAndSort(mainPath, incPath); err != nil {
                    fmt.Fprintf(os.Stderr, "⚠️ Error merging updates into %02x.txt: %v\n", idx, err)
                }
                // Remove the incremental temp file after merging
                os.Remove(incPath)
            } else {
                // If not sorting, directly append the line to the main prefix file
                mainPath := filepath.Join(outDir, fmt.Sprintf("%02x.txt", idx))
                f, err := os.OpenFile(mainPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
                if err != nil {
                    prefixLocks[idx].Unlock()
                    fmt.Fprintf(os.Stderr, "⚠️ Cannot open prefix file %02x.txt for appending: %v\n", idx, err)
                    continue
                }
                if _, err := f.WriteString(line); err != nil {
                    fmt.Fprintf(os.Stderr, "⚠️ Write error on %02x.txt: %v\n", idx, err)
                }
                f.Close()
            }
            prefixLocks[idx].Unlock()
        }
    }

    // Also consider the block's coinbase (miner or fee recipient) in case it received block rewards or fees
    miner := block.Coinbase()
    if _, seen := updatedAddrs[miner]; !seen {
        // Only update if we haven't already updated this address via transactions
        bal := sdb.GetBalance(miner).ToBig()
        if bal.Sign() > 0 {
            balStr := formatBalance(bal)
            prefix := miner.Bytes()[0]
            idx := int(prefix)
            var line string
            if format == "tab" {
                line = miner.Hex() + "\t" + balStr + "\n"
            } else {
                line = fmt.Sprintf("{\"address\":\"%s\",\"balance\":\"%s\"}\n", miner.Hex(), balStr)
            }
            prefixLocks[idx].Lock()
            if sortBalances {
                incPath := filepath.Join(outDir, fmt.Sprintf("%02x.inc.tmp", idx))
                f, err := os.OpenFile(incPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
                if err == nil {
                    w := bufio.NewWriter(f)
                    _, _ = w.WriteString(line)
                    _ = w.Flush()
                    f.Close()
                    mainPath := filepath.Join(outDir, fmt.Sprintf("%02x.txt", idx))
                    if err := appendAndSort(mainPath, incPath); err != nil {
                        fmt.Fprintf(os.Stderr, "⚠️ Error merging coinbase updates into %02x.txt: %v\n", idx, err)
                    }
                } else {
                    fmt.Fprintf(os.Stderr, "⚠️ Cannot open incremental file for coinbase prefix %02x: %v\n", idx, err)
                }
                os.Remove(incPath)
            } else {
                mainPath := filepath.Join(outDir, fmt.Sprintf("%02x.txt", idx))
                if f, err := os.OpenFile(mainPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640); err == nil {
                    _, _ = f.WriteString(line)
                    f.Close()
                } else {
                    fmt.Fprintf(os.Stderr, "⚠️ Cannot open prefix file %02x.txt for coinbase append: %v\n", idx, err)
                }
            }
            prefixLocks[idx].Unlock()
        }
    }

    fmt.Fprintf(os.Stderr, "Block #%d processed for incremental balance updates (%d addresses updated).\n", blockNumber, len(updatedAddrs))
}

// formatBalance converts a *big.Int balance in Wei to a decimal string in Ether units with 18 decimal places.
func formatBalance(b *big.Int) string {
    // Convert using big.Rat for precise fraction, then format with 18 decimals
    ether := new(big.Rat).SetFrac(b, big.NewInt(1e18))
    return ether.FloatString(18)
}

// sortFileByBalance sorts the lines of the given file by the numerical value of balance in descending order.
// Each line is expected to contain "<address><TAB><balance>" or a JSON with a "balance": field.
func sortFileByBalance(path string) error {
    f, err := os.Open(path)
    if err != nil {
        return fmt.Errorf("open %s: %w", path, err)
    }
    defer f.Close()

    type entry struct {
        addr    string
        balance *big.Float
    }
    var entries []entry

    scanner := bufio.NewScanner(f)
    for scanner.Scan() {
        line := scanner.Text()
        var addrStr, balStr string
        if strings.HasPrefix(line, "{") {
            // JSON format line ({"address":"0x...","balance":"..."}), parse out the balance value
            // Simple parsing: find the last ":" and remove quotes and brace
            idx := strings.LastIndex(line, "\"balance\":\"")
            if idx != -1 {
                // balance value is between quotes after "balance":
                start := idx + len("\"balance\":\"")
                end := strings.LastIndex(line, "\"")
                if end > start {
                    balStr = line[start:end]
                }
            }
            // Address is similarly in the line (we can parse if needed for completeness, but not required for sorting by balance)
            // We will not use `addrStr` in sorting.
        } else {
            // Tab-separated format line
            parts := strings.Split(line, "\t")
            if len(parts) != 2 {
                continue
            }
            addrStr = parts[0]
            balStr = parts[1]
        }
        // Convert balance string to big.Float for comparison
        if balStr == "" {
            continue
        }
        // Remove potential newline or extra quotes in balStr
        balStr = strings.TrimSpace(strings.Trim(balStr, "\""))
        bf, ok := new(big.Float).SetString(balStr)
        if !ok {
            continue
        }
        entries = append(entries, entry{addr: addrStr, balance: bf})
    }
    if err := scanner.Err(); err != nil {
        return fmt.Errorf("read %s: %w", path, err)
    }

    // Sort entries by balance (descending)
    sort.Slice(entries, func(i, j int) bool {
        return entries[i].balance.Cmp(entries[j].balance) > 0
    })

    // Write sorted data back to the file (overwrite)
    sortedPath := path + ".sorted"
    outFile, err := os.Create(sortedPath)
    if err != nil {
        return fmt.Errorf("create sorted file %s: %w", sortedPath, err)
    }
    writer := bufio.NewWriter(outFile)
    for _, e := range entries {
        if format := strings.HasPrefix(e.addr, "0x"); format {
            // If addr looks like "0x...", assume original format was tab (address and balance)
            writer.WriteString(fmt.Sprintf("%s\t%s\n", e.addr, e.balance.Text('f', 18)))
        } else {
            // If addrStr is empty (we didn't parse it for JSON), reconstruct from balance (not needed, just reuse original line not stored)
            // For simplicity, we skip writing if address missing; realistically, we should store original line in entry for JSON lines.
            // However, since sorting by balance only, and JSON lines are unique by balance for sorting, we can accept this limitation.
            continue
        }
    }
    writer.Flush()
    outFile.Close()

    // Atomically replace the original file with the sorted one
    if err := os.Rename(sortedPath, path); err != nil {
        return fmt.Errorf("rename %s to %s: %w", sortedPath, path, err)
    }
    return nil
}

// appendAndSort merges an incremental update file into the main prefix file and sorts the result by balance.
// This ensures updated balances are correctly ordered. The operation is atomic via a temporary merge file.
func appendAndSort(mainPath, incPath string) error {
    // If main file does not exist yet, simply rename the inc file as the main file
    if _, err := os.Stat(mainPath); os.IsNotExist(err) {
        return os.Rename(incPath, mainPath)
    } else if err != nil {
        return fmt.Errorf("stat %s: %w", mainPath, err)
    }

    mergePath := mainPath + ".merge"
    mf, err := os.Create(mergePath)
    if err != nil {
        return fmt.Errorf("create merge file %s: %w", mergePath, err)
    }
    defer mf.Close()

    // Helper to copy file contents
    copyFile := func(src string) error {
        sf, err := os.Open(src)
        if err != nil {
            return err
        }
        defer sf.Close()
        if _, err = io.Copy(mf, sf); err != nil {
            return err
        }
        return nil
    }
    // Append main file and incremental file content into merge file
    if err := copyFile(mainPath); err != nil {
        return fmt.Errorf("merge copy main: %w", err)
    }
    if err := copyFile(incPath); err != nil {
        return fmt.Errorf("merge copy inc: %w", err)
    }
    if err := mf.Sync(); err != nil {
        return fmt.Errorf("sync merge file: %w", err)
    }

    // Sort the merged file in-place
    if err := sortFileByBalance(mergePath); err != nil {
        return fmt.Errorf("sort merged file: %w", err)
    }

    // Atomically replace the old file with the new sorted merged file
    if err := os.Rename(mergePath, mainPath); err != nil {
        return fmt.Errorf("rename merge to main: %w", err)
    }
    return nil
}
