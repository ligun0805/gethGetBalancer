package main

import (
	"bufio"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/log"
)

// StartBalanceDumper starts the background balance dumper.
func StartBalanceDumper(ethService *eth.Ethereum, filePath string) {
	go func() {
		for {
			prog := ethService.Downloader().Progress()
			// if the blocks are synchronized AND all state snapshots are downloaded - exit
			if prog.CurrentBlock >= prog.HighestBlock && prog.PulledStates >= prog.KnownStates {
				break
			}
			log.Info("Waiting for snap sync",
				"blocks", fmt.Sprintf("%d/%d", prog.CurrentBlock, prog.HighestBlock),
				"states", fmt.Sprintf("%d/%d", prog.PulledStates, prog.KnownStates),
			)
			time.Sleep(30 * time.Second)
		}

		ticker := time.NewTicker(3600 * time.Second)
		defer ticker.Stop()

		// Perform dump immediately on startup
		dumpWithTimestamp(ethService, filePath)

		for range ticker.C {
			dumpWithTimestamp(ethService, filePath)
		}
	}()
}

func dumpWithTimestamp(ethService *eth.Ethereum, filePath string) {
	dir := filepath.Dir(filePath)
	base := filepath.Base(filePath)
	ext := filepath.Ext(base)
	name := strings.TrimSuffix(base, ext)

	ts := time.Now().UTC().Format("20060102-150405")
	out := filepath.Join(dir, fmt.Sprintf("%s-%s%s", name, ts, ext))

	dumpBalances(ethService, out)
}

// dumpBalances gets the current state of the chain and stores non-zero balances.
func dumpBalances(ethService *eth.Ethereum, filePath string) {
	bc := ethService.BlockChain()
	if bc == nil {
		log.Error("Ethereum service has no blockchain available")
		return
	}
	stateDB, err := bc.State()
	if err != nil {
		log.Error("Failed to get blockchain state", "err", err)
		return
	}
	// Create or overwrite the file
	file, err := os.Create(filePath)
	if err != nil {
		log.Error("Failed to create balance file", "path", filePath, "err", err)
		return
	}
	defer file.Close()
	writer := bufio.NewWriter(file)

	// Dump configuration: skip code and storage, only addresses needed
	cfg := &state.DumpConfig{
		SkipCode:          true,
		SkipStorage:       true,
		OnlyWithAddresses: true,
	}
	collector := &balanceCollector{writer: writer}
	// Go through accounts via DumpToCollector
	stateDB.DumpToCollector(collector, cfg)

	writer.Flush()
	log.Info("Balance dump completed", "file", filePath)
}

// balanceCollector implements state.DumpCollector for handling accounts.
type balanceCollector struct {
	writer *bufio.Writer
	count  int
}

// OnRoot is not used, but is required by the interface.
func (c *balanceCollector) OnRoot(root common.Hash) {}

// OnAccount is called for each account in the state.
func (c *balanceCollector) OnAccount(addr *common.Address, acct state.DumpAccount) {
	if addr == nil {
		return
	}
	// Parse balance (string in decimal system)
	bal, ok := new(big.Int).SetString(acct.Balance, 10)
	if !ok || bal.Sign() <= 0 {
		return
	}
	// Convert balance from wei to ETH with 6 decimal places
	ethInt := new(big.Int).Div(bal, big.NewInt(1e18))
	rem := new(big.Int).Mod(bal, big.NewInt(1e18))
	frac := new(big.Int).Mul(rem, big.NewInt(1000000))
	frac.Div(frac, big.NewInt(1e18))
	balanceStr := fmt.Sprintf("%s.%06d", ethInt.String(), frac.Int64())
	c.writer.WriteString(addr.String() + " " + balanceStr + " ETH\n")
	c.count++
}
