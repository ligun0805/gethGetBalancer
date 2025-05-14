// +build !js
//go:build !js

package main

import (
    // Standard library imports
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

    // Ethereum-specific imports
    "github.com/ethereum/go-ethereum/cmd/utils"
    "github.com/ethereum/go-ethereum/common"
    "github.com/ethereum/go-ethereum/core"
    "github.com/ethereum/go-ethereum/core/types"
    "github.com/ethereum/go-ethereum/eth"
    "github.com/ethereum/go-ethereum/node"
    "github.com/ethereum/go-ethereum/trie"

    // Metrics and CLI
    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/promhttp"
    "github.com/urfave/cli/v2"
)

// prefixLocks ensures file operations per prefix are synchronized
var prefixLocks [256]sync.Mutex

// Prometheus metrics for monitoring
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

// dumpBalancesCommand is the CLI command for dumping balances
var dumpBalancesCommand = &cli.Command{
    Name:  "dump-balances",
    Usage: "full dump balances by prefix, then incremental updates",
    Flags: []cli.Flag{
        utils.DataDirFlag,
        utils.NetworkIdFlag,
        &cli.StringFlag{Name: "out", Aliases: []string{"o"}, Usage: "Output directory for dumps", Required: true},
    },
    Action: func(ctx *cli.Context) error {
        dataDir := ctx.String(utils.DataDirFlag.Name)
        networkID := ctx.Uint64(utils.NetworkIdFlag.Name)
        outDir := ctx.String("out")

        // Ensure output directory exists
        if err := os.MkdirAll(outDir, 0755); err != nil {
            return fmt.Errorf("create output dir: %w", err)
        }

        // Register Prometheus metrics and start HTTP server
        prometheus.MustRegister(accountsProcessed, fullDumpDuration)
        srv := &http.Server{Addr: ":9090", Handler: promhttp.Handler()}
        go func() {
            if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
                fmt.Fprintf(os.Stderr, "metrics server error: %v\n", err)
            }
        }()

        // Connect to Ethereum node service
        _, service, err := connectEthereum(dataDir, networkID)
        if err != nil {
            return err
        }

        // Setup graceful shutdown on signals
        sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
        defer stop()

        // Perform full dump
        dumpAllByPrefix(service, outDir)

        // Subscribe to new blocks for incremental updates
        headCh := make(chan core.ChainHeadEvent)
        sub := service.BlockChain().SubscribeChainHeadEvent(headCh)
        defer sub.Unsubscribe()

        // Process events
        for {
            select {
            case ev := <-headCh:
                if header := ev.Header; header != nil {
                    if blk := service.BlockChain().GetBlockByHash(header.Hash()); blk != nil {
                        incrementalUpdate(service, blk, outDir)
                    }
                }
            case <-sigCtx.Done():
                // Graceful shutdown metrics server
                shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
                defer cancel()
                _ = srv.Shutdown(shutdownCtx)
                return nil
            }
        }
    },
}

// connectEthereum initializes the node and Ethereum service
func connectEthereum(dataDir string, networkID uint64) (*node.Node, *eth.Ethereum, error) {
    cfg := &node.Config{DataDir: dataDir}
    stack, err := node.New(cfg)
    if err != nil {
        return nil, nil, fmt.Errorf("node.New: %w", err)
    }
    ecfg := &eth.Config{NetworkId: networkID}
    service, err := eth.New(stack, ecfg)
    if err != nil {
        return nil, nil, fmt.Errorf("eth.New: %w", err)
    }
    if err := stack.Start(); err != nil {
        return nil, nil, fmt.Errorf("stack.Start: %w", err)
    }
    return stack, service, nil
}

// dumpAllByPrefix performs a full dump of balances grouped by prefix
func dumpAllByPrefix(service *eth.Ethereum, outDir string) {
    start := time.Now()
    writers := make([]*bufio.Writer, 256)
    files := make([]*os.File, 256)

    head := service.BlockChain().CurrentHeader()
    stateDB, err := service.BlockChain().StateAt(head.Root)
    if err != nil {
        fmt.Fprintf(os.Stderr, "StateAt error: %v\n", err)
        return
    }
    tr, err := trie.New(trie.StateTrieID(head.Root), service.BlockChain().TrieDB())
    if err != nil {
        fmt.Fprintf(os.Stderr, "new trie: %v\n", err)
        return
    }
    nodeIt, err := tr.NodeIterator(nil)
    if err != nil {
        fmt.Fprintf(os.Stderr, "trie iterator init: %v\n", err)
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
        balStr := formatBalance(bal)
        p := int(key[0])

        prefixLocks[p].Lock()
        if writers[p] == nil {
            tmp := filepath.Join(outDir, fmt.Sprintf("%02x.tmp", p))
            f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0640)
            if err != nil {
                prefixLocks[p].Unlock()
                continue
            }
            files[p] = f
            writers[p] = bufio.NewWriter(f)
        }
        writers[p].WriteString(addr.Hex() + "\t" + balStr + "\n")
        prefixLocks[p].Unlock()
    }

    // Finalize each prefix
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
            fmt.Fprintf(os.Stderr, "rename error %s -> %s: %v\n", tmp, final, err)
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
            f, err := os.OpenFile(tmp, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0640)
            if err != nil {
                prefixLocks[p].Unlock()
                continue
            }
            w := bufio.NewWriter(f)
            w.WriteString(addr.Hex() + "\t" + balStr + "\n")
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

// sortFileByBalance reads the file, sorts by balance descending, and rewrites it
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
        if !ok { continue }
        lines = append(lines, line{addr: a, bal: r})
    }
    if err := scanner.Err(); err != nil {
        return fmt.Errorf("scan %s: %w", path, err)
    }

    sort.Slice(lines, func(i, j int) bool { return lines[i].bal.Cmp(lines[j].bal) > 0 })

    sortedPath := path + ".sorted"
    f2, err := os.Create(sortedPath)
    if err != nil { return fmt.Errorf("create sorted %s: %w", sortedPath, err) }
    defer f2.Close()
    w2 := bufio.NewWriter(f2)
    for _, l := range lines {
        w2.WriteString(l.addr + "\t" + l.bal.FloatString(18) + "\n")
    }
    if err := w2.Flush(); err != nil { return fmt.Errorf("flush sorted %s: %w", sortedPath, err) }
    if err := os.Rename(sortedPath, path); err != nil {
        return fmt.Errorf("rename %s -> %s: %w", sortedPath, path, err)
    }
    return nil
}

// appendAndSort merges temporary file into main file, sorts, and atomically replaces
func appendAndSort(mainPath, tmpPath string) error {
    // If main doesn't exist, rename tmp → main
    if _, err := os.Stat(mainPath); os.IsNotExist(err) {
        return os.Rename(tmpPath, mainPath)
    } else if err != nil {
        return fmt.Errorf("stat %s: %w", mainPath, err)
    }
    mergePath := mainPath + ".merge"
    mf, err := os.Create(mergePath)
    if err != nil { return fmt.Errorf("create merge %s: %w", mergePath, err) }
    defer mf.Close()

    copyFile := func(dst *os.File, src string) error {
        s, err := os.Open(src)
        if err != nil { return err }
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
        return fmt.Errorf("rename merge %s -> %s: %w", mergePath, mainPath, err)
    }
    if err := os.Remove(tmpPath); err != nil {
        return fmt.Errorf("remove tmp %s: %w", tmpPath, err)
    }
    return nil
}
