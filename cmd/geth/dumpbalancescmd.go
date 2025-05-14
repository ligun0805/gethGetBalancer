package main

import (
	"bufio"
	"fmt"
	"math/big"
	"os"
	"sort"
	"time"

	"github.com/ethereum/go-ethereum/cmd/utils"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/node"
	"github.com/urfave/cli/v2"
)

// dumpBalancesCommand integrates into Geth to sync and dump balances periodically.
var dumpBalancesCommand = &cli.Command{
	Name:  "dump-balances",
	Usage: "Wait for full sync, then periodically dump non-zero account balances",
	Flags: []cli.Flag{
		utils.DataDirFlag,
		utils.NetworkIdFlag,
		&cli.StringFlag{Name: "out", Aliases: []string{"o"}, Usage: "Output file prefix"},
		&cli.DurationFlag{Name: "interval", Usage: "Interval between dumps (e.g. 1h)", Value: time.Hour},
		&cli.BoolFlag{Name: "verbose", Aliases: []string{"v"}, Usage: "Enable verbose logging"},
	},
	Action: runDumpBalances,
}

func init() {
	app.Commands = append(app.Commands, dumpBalancesCommand)
	sort.Sort(cli.CommandsByName(app.Commands))
}

// runDumpBalances starts the node, waits for sync, then dumps balances periodically.
func runDumpBalances(ctx *cli.Context) error {
	// 1) connect and start node
	stack, service, err := connectEthereum(
		ctx.String(utils.DataDirFlag.Name),
		ctx.Uint64(utils.NetworkIdFlag.Name),
	)
	if err != nil {
		return err
	}
	defer stack.Close()

	out := ctx.String("out")
	interval := ctx.Duration("interval")
	verbose := ctx.Bool("verbose")

	if verbose {
		fmt.Printf("DataDir=%s NetworkID=%d Out=%s Interval=%s\n",
			ctx.String(utils.DataDirFlag.Name), ctx.Uint64(utils.NetworkIdFlag.Name), out, interval)
	}

	// 2) wait initial sync
	waitForSync(service)
	fmt.Println("✅ Node fully synced. Starting dumps…")

	// 3) initial dump immediately after sync
	latestRoot := service.BlockChain().CurrentHeader().Root
	fmt.Printf("⏎ [Dump] Initial dump at root: %s\n", latestRoot.Hex())
	if err := doDump(service, latestRoot, out); err != nil {
		fmt.Fprintf(os.Stderr, "❌ initial dump error: %v\n", err)
	}

	// 4) subscribe to new head events
	headCh := make(chan core.ChainHeadEvent)
	sub := service.BlockChain().SubscribeChainHeadEvent(headCh)
	defer sub.Unsubscribe()

	// 5) ticker for periodic dumps
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// 6) event loop: update root on headCh, dump on ticker
	for {
		select {
		case ev := <-headCh:
			latestRoot = ev.Header.Root
			if verbose {
				fmt.Printf("🆕 New head event: root=%s\n", latestRoot.Hex())
			}

		case <-ticker.C:
			fmt.Printf("⏎ [Dump] Ticker dump at root: %s\n", latestRoot.Hex())
			if err := doDump(service, latestRoot, out); err != nil {
				fmt.Fprintf(os.Stderr, "❌ ticker dump error: %v\n", err)
			}
		}
	}
}

// doDump performs the fetchBalances and writeBalances for a given root.
func doDump(service *eth.Ethereum, root common.Hash, out string) error {
	// get state at root
	stateDB, err := service.BlockChain().StateAt(root)
	if err != nil {
		return fmt.Errorf("StateAt: %w", err)
	}
	// fetch balances
	fmt.Println("... fetching balances ...")
	entries, err := fetchBalances(stateDB)
	if err != nil {
		return fmt.Errorf("fetchBalances: %w", err)
	}
	fmt.Printf("✅ fetched %d non-zero balances\n", len(entries))
	// write to file
	filename := makeTimestampFilename(out)
	fmt.Printf("... writing %d entries to %s ...\n", len(entries), filename)
	if err := writeBalances(filename, entries); err != nil {
		return fmt.Errorf("writeBalances: %w", err)
	}
	fmt.Printf("✅ Dump saved: %s\n", filename)
	return nil
}

// waitForSync blocks until initial chain sync completes.
func waitForSync(service *eth.Ethereum) {
	for {
		prog := service.Downloader().Progress()
		if prog.CurrentBlock >= prog.HighestBlock {
			return
		}
		fmt.Println("⏳ Waiting for sync...")
		time.Sleep(10 * time.Second)
	}
}

// connectEthereum starts a Geth node and Eth service.
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

// accountEntry holds an address and its balance.
type accountEntry struct {
	Address common.Address
	Balance *big.Int
}

// fetchBalances scans all accounts and returns non-zero balances sorted.
func fetchBalances(stateDB *state.StateDB) ([]accountEntry, error) {
	dump := stateDB.RawDump(&state.DumpConfig{SkipCode: true, SkipStorage: true})
	res := make([]accountEntry, 0, len(dump.Accounts))
	for addrStr, acc := range dump.Accounts {
		bal, _ := new(big.Int).SetString(acc.Balance, 10)
		if bal.Sign() > 0 {
			res = append(res, accountEntry{common.HexToAddress(addrStr), bal})
		}
	}
	sort.Slice(res, func(i, j int) bool {
		return res[i].Balance.Cmp(res[j].Balance) > 0
	})
	return res, nil
}

// makeTimestampFilename builds a filename with prefix and timestamp.
func makeTimestampFilename(prefix string) string {
	return fmt.Sprintf("%s_%s.txt", prefix, time.Now().Format("20060102_150405"))
}

// writeBalances writes each account and ETH balance to the given file.
func writeBalances(path string, entries []accountEntry) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	defer w.Flush()
	for _, e := range entries {
		val := new(big.Float).Quo(new(big.Float).SetInt(e.Balance), big.NewFloat(1e18))
		if _, err := w.WriteString(fmt.Sprintf("%s\t%s\n", e.Address.Hex(), val.Text('f', 6))); err != nil {
			return err
		}
	}
	return nil
}
