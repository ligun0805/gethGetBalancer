package main

import (
	"fmt"
	"math/big"
	"os"
	"sort"

	"github.com/urfave/cli/v2"

	"github.com/ethereum/go-ethereum/cmd/utils"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/node"
)

var dumpBalancesCommand = &cli.Command{
	Name:  "dump-balances",
	Usage: "Export all non-zero accounts from current state to addresses_balances.txt",
	Flags: []cli.Flag{
		utils.DataDirFlag,
		utils.NetworkIdFlag,
	},
	Action: dumpBalances,
}

func init() {
	commands = append(commands, dumpBalancesCommand)
	sort.Sort(cli.CommandsByName(commands))
}

func dumpBalances(ctx *cli.Context) error {
	cfg := &node.Config{DataDir: ctx.String(utils.DataDirFlag.Name)}
	stack, err := node.New(cfg)
	if err != nil {
		return fmt.Errorf("node.New failed: %w", err)
	}
	ethCfg := &eth.Config{NetworkId: ctx.Uint64(utils.NetworkIdFlag.Name)}
	service, err := eth.New(stack, ethCfg)
	if err != nil {
		return fmt.Errorf("eth.New failed: %w", err)
	}
	stack.RegisterLifecycle(service)
	if err := stack.Start(); err != nil {
		return fmt.Errorf("node.Start failed: %w", err)
	}
	defer stack.Close()

	header := service.BlockChain().Header()
	root := header.Root

	stateDB, err := service.BlockChain().StateAt(root)
	if err != nil {
		return fmt.Errorf("StateAt failed: %w", err)
	}

	dump := stateDB.RawDump(&state.DumpConfig{
		SkipCode:    true,
		SkipStorage: true,
	})

	type entry struct {
		addr common.Address
		bal  *big.Int
	}
	var list []entry
	for _, acc := range dump.Accounts {
		if acc.Address == nil {
			continue
		}
		bi := new(big.Int)
		bi.SetString(acc.Balance, 10)
		if bi.Sign() > 0 {
			list = append(list, entry{*acc.Address, bi})
		}
	}

	sort.Slice(list, func(i, j int) bool {
		return list[i].bal.Cmp(list[j].bal) > 0
	})

	f, err := os.Create("addresses_balances.txt")
	if err != nil {
		return fmt.Errorf("cannot create file: %w", err)
	}
	defer f.Close()
	for _, e := range list {
		ethVal := new(big.Float).Quo(new(big.Float).SetInt(e.bal), big.NewFloat(1e18))
		fmt.Fprintf(f, "%s\t%.6f\n", e.addr.Hex(), ethVal)
	}

	fmt.Println("✅ Dump completed: addresses_balances.txt")
	return nil
}
