// +build !js
//go:build !js
// +build !js

package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"math/big"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/urfave/cli/v2"
)

// Global verbose flag for logging (set by --verbose)
var verboseLog bool

func main() {
	app := cli.NewApp()
	app.Name = "dumpbalances-ipc"
	app.Usage = "Dump all non-zero Ethereum address balances via IPC"
	app.Flags = []cli.Flag{
		&cli.StringFlag{Name: "ipcpath", Usage: "Path to Geth IPC socket file", Required: true},
		&cli.StringFlag{Name: "out", Aliases: []string{"o"}, Usage: "Output directory for dumps", Required: true},
		&cli.BoolFlag{Name: "verbose", Aliases: []string{"v"}, Usage: "Enable verbose logging"},
	}
	app.Action = func(ctx *cli.Context) error {
		verboseLog = ctx.Bool("verbose")
		ipcPath := ctx.String("ipcpath")
		outDir := ctx.String("out")
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			return fmt.Errorf("create output dir: %w", err)
		}

		// Establish IPC connection to geth node
		client, err := rpc.DialIPC(context.Background(), ipcPath)
		if err != nil {
			return fmt.Errorf("IPC dial failed: %w", err)
		}
		defer client.Close()
		// Wrap RPC client with ethclient for convenience
		ethCl := ethclient.NewClient(client)
		defer ethCl.Close()

		// Setup graceful shutdown on SIGINT/SIGTERM
		sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()

		// 1. Perform full state dump via debug_dumpBlock("latest")
		log.Println("▶ Starting full state dump via debug_dumpBlock(latest)...")
		var dumpResult []struct {
			Address string `json:"address"`
			Balance string `json:"balance"`
		}
		// Use RPC call to debug_dumpBlock for latest block state
		if err := client.CallContext(sigCtx, &dumpResult, "debug_dumpBlock", "latest"); err != nil {
			return fmt.Errorf("debug_dumpBlock RPC call failed: %w", err)
		}
		log.Printf("Retrieved %d accounts from state dump", len(dumpResult))

		// Prepare writers for each prefix (00-ff)
		writers := make(map[int]*bufio.Writer)
		files := make(map[int]*os.File)
		// Write each account to its prefix file (temporary)
		for i, acct := range dumpResult {
			// Convert balance from hex string to big.Int
			bal := new(big.Int)
			if len(acct.Balance) > 2 {
				// Assuming hex string (e.g. "0x5f5e100")
				bal.SetString(acct.Balance[2:], 16)
			}
			if bal.Sign() == 0 {
				continue // skip zero-balance accounts
			}
			addr := common.HexToAddress(acct.Address)
			p := int(addr.Bytes()[0])
			// Open file for this prefix if not already
			if writers[p] == nil {
				tmpPath := filepath.Join(outDir, fmt.Sprintf("%02x.tmp", p))
				f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
				if err != nil {
					log.Printf("Error opening prefix file %02x.tmp: %v", p, err)
					continue
				}
				files[p] = f
				writers[p] = bufio.NewWriter(f)
			}
			// Write address and balance (in ETH with 18 decimals) to file
			line := addr.Hex() + "\t" + formatBalance(bal) + "\n"
			if _, err := writers[p].WriteString(line); err != nil {
				log.Printf("Write error for prefix %02x: %v", p, err)
			}
			if verboseLog && i%100000 == 0 {
				log.Printf("... %d accounts processed", i)
			}
		}
		// Flush and close all prefix files, then sort and rename to final .txt
		for p, w := range writers {
			_ = w.Flush()
			_ = files[p].Close()
			tmpPath := filepath.Join(outDir, fmt.Sprintf("%02x.tmp", p))
			mainPath := filepath.Join(outDir, fmt.Sprintf("%02x.txt", p))
			if err := sortFileByBalance(tmpPath); err != nil {
				log.Printf("Error sorting %02x.tmp: %v", p, err)
			}
			if err := os.Rename(tmpPath, mainPath); err != nil {
				log.Printf("Error renaming %02x.tmp to %02x.txt: %v", p, err)
			}
		}
		log.Println("✅ Full state dump complete. Watching for new blocks...")

		// 2. Subscribe to new block headers for incremental updates
		headersCh := make(chan *types.Header)
		sub, err := ethCl.SubscribeNewHead(sigCtx, headersCh)
		if err != nil {
			return fmt.Errorf("SubscribeNewHead failed: %w", err)
		}
		defer sub.Unsubscribe()

		// Determine chain ID for signer
		chainID, err := ethCl.ChainID(sigCtx)
		if err != nil {
			return fmt.Errorf("Failed to get chain ID: %w", err)
		}
		signer := types.NewEIP155Signer(chainID)

		// 3. Event loop for new blocks
		for {
			select {
			case header := <-headersCh:
				if header == nil {
					continue
				}
				if verboseLog {
					log.Printf("🔄 New block #%d: %s", header.Number, header.Hash().Hex())
				}
				// Fetch the full block to get transactions
				block, err := ethCl.BlockByHash(sigCtx, header.Hash())
				if err != nil {
					log.Printf("Failed to fetch block %s: %v", header.Hash(), err)
					continue
				}
				// Apply incremental updates for this block
				incrementalUpdateRPC(ethCl, signer, block, outDir)
			case err := <-sub.Err():
				return fmt.Errorf("Subscription error: %w", err)
			case <-sigCtx.Done():
				// graceful exit on signal
				log.Println("Shutdown signal received, terminating...")
				return nil
			}
		}
	}
	if err := app.Run(os.Args); err != nil {
		log.Fatalf("Error: %v", err)
	}
}

// incrementalUpdateRPC processes a new block to update address balances
func incrementalUpdateRPC(ethCl *ethclient.Client, signer types.Signer, block *types.Block, outDir string) {
	// Maps for addresses to update and to remove
	updates := make(map[int]map[string]*big.Int)
	zeros := make(map[int]map[string]struct{})

	// Determine addresses affected by this block's transactions
	for _, tx := range block.Transactions() {
		// Get sender address from the transaction
		msg, err := tx.AsMessage(signer, nil)
		var from common.Address
		if err == nil {
			from = msg.From()
		}
		addrs := []common.Address{}
		if err == nil {
			addrs = append(addrs, from)
		}
		if to := tx.To(); to != nil {
			addrs = append(addrs, *to)
		}
		// For each relevant address, query the latest balance at this block
		for _, addr := range addrs {
			// Use RPC to get balance of addr at block height
			bal, err := ethCl.BalanceAt(context.Background(), addr, block.Number())
			if err != nil {
				log.Printf("BalanceAt RPC error for %s: %v", addr.Hex(), err)
				continue
			}
			p := int(addr.Bytes()[0])
			addrHex := addr.Hex()
			if bal.Sign() == 0 {
				// If balance is zero, mark for removal (and remove any pending update)
				if updates[p] != nil {
					delete(updates[p], addrHex)
				}
				if zeros[p] == nil {
					zeros[p] = make(map[string]struct{})
				}
				zeros[p][addrHex] = struct{}{}
			} else {
				// Non-zero balance: add to updates
				if updates[p] == nil {
					updates[p] = make(map[string]*big.Int)
				}
				updates[p][addrHex] = bal
				// If this address was also marked zero earlier, undo that
				if zeros[p] != nil {
					delete(zeros[p], addrHex)
				}
			}
		}
	}

	// Write updates per prefix to .inc.tmp files and merge into main files
	var wg sync.WaitGroup
	for p, addrMap := range updates {
		wg.Add(1)
		go func(prefix int, addrMap map[string]*big.Int) {
			defer wg.Done()
			tmpPath := filepath.Join(outDir, fmt.Sprintf("%02x.inc.tmp", prefix))
			f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
			if err != nil {
				log.Printf("Error creating %02x.inc.tmp: %v", prefix, err)
				return
			}
			w := bufio.NewWriter(f)
			for addrHex, bal := range addrMap {
				_, _ = w.WriteString(addrHex + "\t" + formatBalance(bal) + "\n")
			}
			_ = w.Flush()
			_ = f.Close()
			mainPath := filepath.Join(outDir, fmt.Sprintf("%02x.txt", prefix))
			if err := appendAndSort(mainPath, tmpPath); err != nil {
				log.Printf("appendAndSort error for prefix %02x: %v", prefix, err)
			}
			if verboseLog {
				log.Printf("Updated %d addresses in prefix %02x", len(addrMap), prefix)
			}
		}(p, addrMap)
	}
	wg.Wait()

	// Remove zero-balance addresses from files
	for p, zeroMap := range zeros {
		if len(zeroMap) == 0 {
			continue
		}
		addrs := make([]common.Address, 0, len(zeroMap))
		for addrHex := range zeroMap {
			addr := common.HexToAddress(addrHex)
			addrs = append(addrs, addr)
		}
		mainPath := filepath.Join(outDir, fmt.Sprintf("%02x.txt", p))
		if err := removeAddresses(mainPath, addrs); err != nil {
			// If file does not exist, it means we had no prior entries for that prefix
			if !os.IsNotExist(err) {
				log.Printf("removeAddresses error for %s: %v", mainPath, err)
			}
		} else if verboseLog {
			log.Printf("Removed %d addresses from prefix %02x", len(addrs), p)
		}
	}
}

// formatBalance converts a balance in Wei (big.Int) to an ETH string with 18 decimal places
func formatBalance(b *big.Int) string {
	// Use big.Rat for precise formatting of large integers
	ether := new(big.Rat).SetFrac(b, big.NewInt(1e18))
	return ether.FloatString(18)
}

// sortFileByBalance sorts the lines of a file by balance in descending order.
// Each line is expected to be "address<TAB>balance".
func sortFileByBalance(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	type entry struct {
		addr string
		bal  *big.Rat
	}
	entries := []entry{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		addrStr, balStr := fields[0], fields[1]
		// Parse balance string to big.Rat
		balRat, ok := new(big.Rat).SetString(balStr)
		if !ok {
			continue
		}
		entries = append(entries, entry{addr: addrStr, bal: balRat})
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan %s: %w", path, err)
	}
	// Sort entries by balance (desc)
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].bal.Cmp(entries[j].bal) > 0
	})
	// Rewrite file with sorted entries
	tmpSorted := path + ".sorted"
	outFile, err := os.Create(tmpSorted)
	if err != nil {
		return fmt.Errorf("create sorted file: %w", err)
	}
	defer outFile.Close()
	writer := bufio.NewWriter(outFile)
	for _, e := range entries {
		_, _ = writer.WriteString(e.addr + "\t" + e.bal.FloatString(18) + "\n")
	}
	_ = writer.Flush()
	// Replace original file
	if err := os.Rename(tmpSorted, path); err != nil {
		return fmt.Errorf("rename sorted file: %w", err)
	}
	return nil
}

// appendAndSort merges a temporary update file into the main file, sorts, and replaces atomically.
func appendAndSort(mainPath, tmpPath string) error {
	// If main file doesn't exist, simply rename tmp to main
	if _, err := os.Stat(mainPath); os.IsNotExist(err) {
		return os.Rename(tmpPath, mainPath)
	}
	mergePath := mainPath + ".merge"
	mf, err := os.Create(mergePath)
	if err != nil {
		return fmt.Errorf("create merge file: %w", err)
	}
	defer mf.Close()
	// Copy main file and tmp file into merge file
	copyFile := func(dst *os.File, srcPath string) error {
		src, err := os.Open(srcPath)
		if err != nil {
			return err
		}
		defer src.Close()
		if _, err := io.Copy(dst, src); err != nil {
			return err
		}
		return nil
	}
	if err := copyFile(mf, mainPath); err != nil {
		return fmt.Errorf("copy main file: %w", err)
	}
	if err := copyFile(mf, tmpPath); err != nil {
		return fmt.Errorf("copy tmp file: %w", err)
	}
	if err := mf.Sync(); err != nil {
		return fmt.Errorf("sync merge file: %w", err)
	}
	// Sort the merged file by balance
	if err := sortFileByBalance(mergePath); err != nil {
		return fmt.Errorf("sort merge: %w", err)
	}
	// Replace main file with sorted merge
	if err := os.Rename(mergePath, mainPath); err != nil {
		return fmt.Errorf("rename merge file: %w", err)
	}
	// Remove temp file
	if err := os.Remove(tmpPath); err != nil {
		return fmt.Errorf("remove tmp file: %w", err)
	}
	return nil
}

// removeAddresses removes any lines corresponding to the given addresses from the prefix file.
func removeAddresses(mainPath string, addrs []common.Address) error {
	if len(addrs) == 0 {
		return nil
	}
	f, err := os.Open(mainPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", mainPath, err)
	}
	defer f.Close()
	// Build a set of addresses to remove (hex strings)
	removeSet := make(map[string]struct{}, len(addrs))
	for _, addr := range addrs {
		removeSet[addr.Hex()] = struct{}{}
	}
	tmpPath := mainPath + ".new"
	out, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("create %s: %w", tmpPath, err)
	}
	defer out.Close()
	scanner := bufio.NewScanner(f)
	writer := bufio.NewWriter(out)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		addr := fields[0]
		if _, shouldRemove := removeSet[addr]; shouldRemove {
			// skip this address (remove it)
			continue
		}
		_, _ = writer.WriteString(line + "\n")
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan %s: %w", mainPath, err)
	}
	_ = writer.Flush()
	// Replace old file with new filtered file
	if err := os.Rename(tmpPath, mainPath); err != nil {
		return fmt.Errorf("rename %s to %s: %w", tmpPath, mainPath, err)
	}
	return nil
}
