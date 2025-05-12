package main

import (
	"bufio"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"

	"github.com/ethereum/go-ethereum/cmd/utils"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/node"
	"github.com/schollz/progressbar/v3"
	"github.com/urfave/cli/v2"
)

var dumpBalancesCommand = &cli.Command{
	Name:  "dump-balances",
	Usage: "Export all non-zero accounts from current state to a file",
	Flags: []cli.Flag{
		utils.DataDirFlag,
		utils.NetworkIdFlag,
		&cli.StringFlag{
			Name:    "out",
			Aliases: []string{"o"},
			Usage:   "Output file path",
		},
		&cli.BoolFlag{
			Name:    "verbose",
			Aliases: []string{"v"},
			Usage:   "Enable verbose logging",
		},
	},
	Action: dumpBalances,
}

func init() {
	app.Commands = append(app.Commands, dumpBalancesCommand)
	sort.Sort(cli.CommandsByName(app.Commands))
}

func dumpBalances(ctx *cli.Context) error {
	// Connect to Ethereum node
	stack, service, err := connectEthereum(
		ctx.String(utils.DataDirFlag.Name),
		ctx.Uint64(utils.NetworkIdFlag.Name),
	)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := stack.Close(); cerr != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to close node: %v\n", cerr)
		}
	}()

	// Get state database at latest header
	stateDB, err := getStateDB(service)
	if err != nil {
		return err
	}

	// Fetch non-zero balances
	entries, err := fetchBalances(stateDB, ctx.Bool("verbose"))
	if err != nil {
		return err
	}

	// Determine output path
	outPath := ctx.String("out")
	if outPath == "" {
		outPath = filepath.Join(ctx.String(utils.DataDirFlag.Name), "addresses_balances.txt")
	}

	// Write results
	if err := writeBalances(outPath, entries); err != nil {
		return err
	}

	fmt.Printf("✅ Dump completed: %s\n", outPath)
	return nil
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

// getStateDB retrieves the state database at the current head
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

// fetchBalances scans all accounts and returns those with non-zero balance
func fetchBalances(stateDB *state.StateDB, verbose bool) ([]accountEntry, error) {
	// Dump all accounts into a map[string]DumpAccount
	dump := stateDB.RawDump(&state.DumpConfig{SkipCode: true, SkipStorage: true})

	// Prepare a determinate progress bar if verbose
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

	// Iterate over the map: key is stringified address
	entries := make([]accountEntry, 0, total)
	for addrStr, acc := range dump.Accounts {
		if verbose {
			bar.Add(1)
		}
		// Parse balance from string to big.Int
		bal, ok := new(big.Int).SetString(acc.Balance, 10)
		if !ok {
			return nil, fmt.Errorf("invalid balance for %s: %s", addrStr, acc.Balance)
		}
		if bal.Sign() > 0 {
			// Convert hex-string back to common.Address
			address := common.HexToAddress(addrStr)
			entries = append(entries, accountEntry{address, bal})
		}
	}

	// Sort descending by balance
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Balance.Cmp(entries[j].Balance) > 0
	})
	return entries, nil
}

// writeBalances writes the sorted balances to the specified file
func writeBalances(path string, entries []accountEntry) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("failed to create output file: %w", err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to close file: %v\n", cerr)
		}
	}()

	// Prepare divisor for Wei to Ether conversion
	bw := bufio.NewWriter(f)
	defer bw.Flush()
	for _, e := range entries {
		if _, err := bw.WriteString(e.Address.Hex() + "\t" + e.Balance.String() + "\n"); err != nil {
			return fmt.Errorf("failed to write to buffer: %w", err)
		}
	}
	return nil
}
