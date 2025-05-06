package main

import (
    "fmt"
    "os"
    "sort"
    "math/big"
    "github.com/urfave/cli/v2"
    "github.com/ethereum/go-ethereum/common"
    cmdutils "github.com/ethereum/go-ethereum/cmd/utils"
    "github.com/ethereum/go-ethereum/core"
    "github.com/ethereum/go-ethereum/core/state"
    "github.com/ethereum/go-ethereum/node"
    "github.com/ethereum/go-ethereum/eth"
)

var dumpBalancesCommand = &cli.Command{
    Name:  "dump-balances",
    Usage: "Dump all accounts with non-zero balance from current state to file",
    Flags: []cli.Flag{
        &cli.StringFlag{
            Name:    "datadir",
            Usage:   "Path to the geth data directory",
            Value:   cmdutils.DefaultDataDir(),
            EnvVars: []string{"GETH_DATADIR"},
        },
        &cli.Uint64Flag{
            Name:    "networkid",
            Usage:   "Network identifier",
            Value:   cmdutils.DefaultNetworkId(),
        },
        &cli.StringFlag{
            Name:  "syncmode",
            Usage: "Synchronization mode (full, fast, snap)",
            Value: cmdutils.DefaultSyncMode(),
        },
    },
    Action: dumpBalances,
}

func init() {
    Commands = append(Commands, dumpBalancesCommand)
    sort.Sort(cli.CommandsByName(Commands))
}

func dumpBalances(ctx *cli.Context) error {
    cfg := &node.Config{DataDir: ctx.String("datadir")}
    stack, err := node.New(cfg)
    if err != nil {
        return fmt.Errorf("node.New failed: %v", err)
    }
    ethCfg := &eth.Config{
        NetworkId: ctx.Uint64("networkid"),
        SyncMode:  core.SyncMode(ctx.String("syncmode")),
    }
    ethService, err := eth.New(stack, ethCfg)
    if err != nil {
        return fmt.Errorf("eth.New failed: %v", err)
    }
    stack.RegisterLifecycle(ethService)
    if err := stack.Start(); err != nil {
        return fmt.Errorf("node.Start failed: %v", err)
    }
    defer stack.Stop()

    chain := ethService.BlockChain()
    head := chain.CurrentBlock()
    stateDB, err := chain.StateAt(head.Root())
    if err != nil {
        return fmt.Errorf("StateAt failed: %v", err)
    }

    dump := stateDB.IteratorDump(&state.DumpConfig{
        SkipCode:    true,
        SkipStorage: true,
    })

    type ab struct {
        addr common.Address
        bal  *big.Int
    }
    var list []ab
    for addr, acc := range dump.Accounts {
        bi := new(big.Int)
        bi.SetString(acc.Balance, 10)
        if bi.Sign() > 0 {
            list = append(list, ab{addr, bi})
        }
    }

    sort.Slice(list, func(i, j int) bool {
        return list[i].bal.Cmp(list[j].bal) > 0
    })

    f, err := os.Create("addresses_balances.txt")
    if err != nil {
        return fmt.Errorf("cannot create file: %v", err)
    }
    defer f.Close()

    for _, item := range list {
        ethVal := new(big.Float).Quo(new(big.Float).SetInt(item.bal), big.NewFloat(1e18))
        fmt.Fprintf(f, "%s\t%.6f\n", item.addr.Hex(), ethVal)
    }

    fmt.Println("✅ Dump completed: addresses_balances.txt")
    return nil
}
