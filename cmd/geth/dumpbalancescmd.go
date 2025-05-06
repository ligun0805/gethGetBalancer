package main

import (
	"fmt"
	"math/big"
	"os"
	"sort"

	"github.com/urfave/cli/v2"

	"github.com/ethereum/go-ethereum/cmd/utils"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/node"
)

var dumpBalancesCommand = &cli.Command{
	Name:  "dump-balances",
	Usage: "Dump all accounts with non-zero balance from current state to file",
	Flags: []cli.Flag{
		utils.DataDirFlag,
		utils.NetworkIdFlag,
		utils.SyncModeFlag,
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
		return err
	}
	ethCfg := &eth.Config{
		NetworkId: ctx.Uint64(utils.NetworkIdFlag.Name),
		SyncMode:  core.SyncMode(ctx.String(utils.SyncModeFlag.Name)),
	}
	ethService, err := eth.New(stack, ethCfg)
	if err != nil {
		return err
	}
	stack.RegisterLifecycle(ethService)
	if err := stack.Start(); err != nil {
		return err
	}
	defer stack.Close()

	chain := ethService.BlockChain()
	head := chain.CurrentBlock()
	root := head.Header().Root

	stateDB, err := chain.StateAt(root)
	if err != nil {
		return err
	}

	dump := stateDB.RawDump(&state.DumpConfig{
		SkipCode:    true,
		SkipStorage: true,
	})

	type acct struct {
		addr common.Address
		bal  *big.Int
	}
	var list []acct
	for _, a := range dump.Accounts {
		if a.Address == nil {
			continue
		}
		b := new(big.Int)
		b.SetString(a.Balance, 10)
		if b.Sign() > 0 {
			list = append(list, acct{*a.Address, b})
		}
	}

	sort.Slice(list, func(i, j int) bool {
		return list[i].bal.Cmp(list[j].bal) > 0
	})

	f, err := os.Create("addresses_balances.txt")
	if err != nil {
		return err
	}
	defer f.Close()
	for _, x := range list {
		ethVal := new(big.Float).Quo(new(big.Float).SetInt(x.bal), big.NewFloat(1e18))
		fmt.Fprintf(f, "%s\t%.6f\n", x.addr.Hex(), ethVal)
	}
	fmt.Println("✅ Dump completed: addresses_balances.txt")
	return nil
}
