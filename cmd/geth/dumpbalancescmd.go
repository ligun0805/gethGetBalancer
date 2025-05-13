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
	"github.com/schollz/progressbar/v3"
	"github.com/urfave/cli/v2"
)

var dumpBalancesCommand = &cli.Command{
	Name:  "dump-balances",
	Usage: "Continuously monitor new blocks and export non-zero accounts at intervals",
	Flags: []cli.Flag{
		utils.DataDirFlag,
		utils.NetworkIdFlag,
		&cli.StringFlag{
			Name:    "out",
			Aliases: []string{"o"},
			Usage:   "Output file prefix",
		},
		&cli.DurationFlag{
			Name:  "interval",
			Usage: "Time interval between dumps (e.g. 1h, 30m)",
			Value: time.Hour,
		},
		&cli.BoolFlag{
			Name:    "verbose",
			Aliases: []string{"v"},
			Usage:   "Enable verbose logging",
		},
	},
	Action: dumpBalancesContinuous,
}

func init() {
	app.Commands = append(app.Commands, dumpBalancesCommand)
	sort.Sort(cli.CommandsByName(app.Commands))
}

func dumpBalancesContinuous(ctx *cli.Context) error {
	stack, service, err := connectEthereum(
		ctx.String(utils.DataDirFlag.Name),
		ctx.Uint64(utils.NetworkIdFlag.Name),
	)
	if err != nil {
		return err
	}
	defer stack.Close()

	outPrefix := ctx.String("out")
	interval := ctx.Duration("interval")
	verbose := ctx.Bool("verbose")

	waitForSync(service)

	fmt.Println("✅ Sync complete. Monitoring new blocks...")

	headCh := make(chan core.ChainHeadEvent)
	sub := service.BlockChain().SubscribeChainHeadEvent(headCh)
	defer sub.Unsubscribe()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var latestRoot common.Hash
	for ev := range headCh {
		latestRoot = ev.Header.Root
		select {
		case <-ticker.C:
			stateDB, err := service.BlockChain().StateAt(latestRoot)
			if err != nil {
				fmt.Fprintf(os.Stderr, "❌ failed to get state: %v\n", err)
				continue
			}
			entries, err := fetchBalances(stateDB, verbose)
			if err != nil {
				fmt.Fprintf(os.Stderr, "❌ fetchBalances error: %v\n", err)
				continue
			}
			filename := makeTimestampFilename(outPrefix)
			if err := writeBalances(filename, entries); err != nil {
				fmt.Fprintf(os.Stderr, "❌ writeBalances error: %v\n", err)
			} else {
				fmt.Printf("✅ Dump saved to %s\n", filename)
			}
		default:
			continue
		}
	}
	return nil
}

func waitForSync(service *eth.Ethereum) {
	for {
		progress := service.Downloader().Progress()
		if progress.CurrentBlock == progress.HighestBlock {
			return
		}
		fmt.Println("⏳ Waiting for sync...")
		time.Sleep(10 * time.Second)
	}
}

func makeTimestampFilename(prefix string) string {
	timestamp := time.Now().Format("20060102_150405")
	return fmt.Sprintf("%s_%s.txt", prefix, timestamp)
}

// connectEthereum initializes and starts an Ethereum node and service
func connectEthereum(dataDir string, networkID uint64) (*node.Node, *eth.Ethereum, error) {
	cfg := &node.Config{DataDir: dataDir}
	stack, err := node.New(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create node: %w", err)
	}

	ethCfg := &eth.Config{NetworkId: networkID}
	service, err := eth.New(stack, ethCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create eth service: %w", err)
	}

	if err := stack.Start(); err != nil {
		return nil, nil, fmt.Errorf("failed to start node: %w", err)
	}
	return stack, service, nil
}

func getStateDB(service *eth.Ethereum) (*state.StateDB, error) {
	header := service.BlockChain().CurrentHeader()
	stateDB, err := service.BlockChain().StateAt(header.Root)
	if err != nil {
		return nil, fmt.Errorf("failed to get state at root: %w", err)
	}
	return stateDB, nil
}

type accountEntry struct {
	Address common.Address
	Balance *big.Int
}

func fetchBalances(stateDB *state.StateDB, verbose bool) ([]accountEntry, error) {
	dump := stateDB.RawDump(&state.DumpConfig{SkipCode: true, SkipStorage: true})
	total := len(dump.Accounts)
	var bar *progressbar.ProgressBar
	if verbose {
		bar = progressbar.NewOptions(total,
			progressbar.OptionEnableColorCodes(false),
			progressbar.OptionShowCount(),
			progressbar.OptionShowDescriptionAtLineEnd(),
			progressbar.OptionSetDescription("Scanning accounts"),
			progressbar.OptionSetWidth(20),
		)
		defer bar.Close()
	}

	entries := make([]accountEntry, 0, total)
	for addrStr, acc := range dump.Accounts {
		if verbose {
			bar.Add(1)
		}
		bal, ok := new(big.Int).SetString(acc.Balance, 10)
		if !ok {
			return nil, fmt.Errorf("invalid balance for %s: %s", addrStr, acc.Balance)
		}
		if bal.Sign() > 0 {
			address := common.HexToAddress(addrStr)
			entries = append(entries, accountEntry{address, bal})
		}
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Balance.Cmp(entries[j].Balance) > 0
	})
	return entries, nil
}

func writeBalances(path string, entries []accountEntry) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("failed to create output file: %w", err)
	}
	defer f.Close()

	bw := bufio.NewWriter(f)
	defer bw.Flush()
	for _, e := range entries {
		eth := new(big.Float).Quo(new(big.Float).SetInt(e.Balance), big.NewFloat(1e18))
		if _, err := bw.WriteString(e.Address.Hex() + "\t" + eth.Text('f', 6) + "\n"); err != nil {
			return fmt.Errorf("failed to write to buffer: %w", err)
		}
	}
	return nil
}
