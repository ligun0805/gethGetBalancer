package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"github.com/streadway/amqp"
)

// Export scans the entire history, collects addresses from transactions,
// filters by balance>0 batch requests and writes them to outFile.
// Logs key steps and progress everywhere.
func Export(ctx context.Context, stack *node.Node, outFile string) error {
	start := time.Now()
	log.Info("Starting address export", "outFile", outFile)

	stateFile := outFile + ".state"
	balancesFile := outFile
	lastProcessed, err := loadProgress(stateFile)
	if err != nil {
		return fmt.Errorf("load progress: %w", err)
	}
	if lastProcessed == 0 {
		lastProcessed = 22546721
	}
	startBlock := lastProcessed + 1
	balances, err := loadBalances(balancesFile)
	if err != nil {
		return fmt.Errorf("load balances: %w", err)
	}

	// 1) in-proc RPC
	log.Info("Attaching to in-process RPC")
	rpcClient := stack.Attach()
	client := ethclient.NewClient(rpcClient)

	// 2) Fetching latest block number
	log.Info("Fetching latest block number")
	header, err := client.HeaderByNumber(ctx, nil)
	if err != nil {
		return fmt.Errorf("get latest header: %w", err)
	}
	latest := header.Number.Uint64()
	log.Info("Latest block", "number", latest)

	// 3) Scan new blocks sequentially and update balances and progress
	log.Info("Scanning blocks", "from", startBlock)
	for num := startBlock; num <= latest; num++ {
		blk, err := client.BlockByNumber(ctx, new(big.Int).SetUint64(num))
		if err != nil {
			log.Warn("Failed to fetch block", "block", num, "err", err)
			continue
		}
		addrSet := make(map[common.Address]struct{})
		signer := types.LatestSignerForChainID(big.NewInt(1))
		for _, tx := range blk.Transactions() {
			// From sender/recipient/contract
			if from, err := types.Sender(signer, tx); err == nil {
				addrSet[from] = struct{}{}
			}
			if to := tx.To(); to != nil {
				addrSet[*to] = struct{}{}
			} else {
				from, _ := types.Sender(signer, tx)
				contract := crypto.CreateAddress(from, tx.Nonce())
				addrSet[contract] = struct{}{}
			}
			// Receipt logs
			rcpt, err := client.TransactionReceipt(ctx, tx.Hash())
			if err != nil {
				log.Warn("Failed to fetch receipt", "tx", tx.Hash().Hex(), "err", err)
			} else {
				for _, l := range rcpt.Logs {
					addrSet[l.Address] = struct{}{}
				}
			}
		}
		// Updating balances
		for addr := range addrSet {
			bal, err := client.BalanceAt(ctx, addr, nil)
			if err != nil {
				log.Warn("Failed to get balance", "addr", addr.Hex(), "err", err)
				continue
			}
			if bal.Cmp(big.NewInt(0)) > 0 {
				balances[addr] = bal
			} else {
				delete(balances, addr)
			}
		}
	}
	// Write updated balance file
	if err := writeBalances(balancesFile, balances); err != nil {
		return fmt.Errorf("write balances: %w", err)
	}
	// Save progress
	if err := saveProgress(stateFile, latest); err != nil {
		return fmt.Errorf("save progress: %w", err)
	}
	log.Info("Export completed", "balancesFile", balancesFile, "lastBlock", latest, "duration", time.Since(start))
	return nil
}

// loadBalances reads a file with balances (each line: "balance address")
func loadBalances(path string) (map[common.Address]*big.Int, error) {
	m := make(map[common.Address]*big.Int)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return m, nil
		}
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		parts := strings.Fields(scanner.Text())
		if len(parts) != 2 {
			continue
		}
		addr := common.HexToAddress(parts[0])
		bal := new(big.Int)
		if _, ok := bal.SetString(parts[1], 10); ok {
			m[addr] = bal
		}
	}
	return m, scanner.Err()
}

// writeBalances writes map[address]balance to file (each line: "address balance")
func writeBalances(path string, m map[common.Address]*big.Int) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	for addr, bal := range m {
		fmt.Fprintf(w, "%s %s\n", addr.Hex(), bal.String())
	}
	return w.Flush()
}

// loadProgress reads the last processed block height from the file
func loadProgress(path string) (uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, nil
	}
	return n, nil
}

// saveProgress saves the height of the last processed block to a file
func saveProgress(path string, n uint64) error {
	return os.WriteFile(path, []byte(strconv.FormatUint(n, 10)), 0644)
}

func IncrementalDump(ctx context.Context, rpcClient *ethclient.Client, outFile string) error {
	stateFile := outFile + ".state"
	balancesFile := outFile

	// 1) Progress
	lastProcessed, err := loadProgress(stateFile)
	if err != nil {
		return fmt.Errorf("load progress: %w", err)
	}
	if lastProcessed == 0 {
		lastProcessed = 22546721
	}

	// 2) Existing balances
	balances, err := loadBalances(balancesFile)
	if err != nil {
		return fmt.Errorf("load balances: %w", err)
	}

	// 3) Catch-up: from lastProcessed+1 to current vertex
	head, err := rpcClient.HeaderByNumber(ctx, nil)
	if err != nil {
		return fmt.Errorf("get latest header: %w", err)
	}
	log.Info("Catch-up incremental dump", "from", lastProcessed+1, "to", head.Number.Uint64())
	for num := lastProcessed + 1; num <= head.Number.Uint64(); num++ {
		if err := processBlock(ctx, rpcClient, num, balances, balancesFile); err != nil {
			log.Warn("process block failed", "block", num, "err", err)
		}
		if err := saveProgress(stateFile, num); err != nil {
			log.Warn("save progress failed", "block", num, "err", err)
		}
	}

	// 4) Real-time: subscription to new blocks
	headers := make(chan *types.Header)
	sub, err := rpcClient.SubscribeNewHead(ctx, headers)
	if err != nil {
		return fmt.Errorf("subscribe new head: %w", err)
	}
	log.Info("Starting real-time incremental dump", "from", lastProcessed+1)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-sub.Err():
			return fmt.Errorf("subscription error: %w", err)
		case header := <-headers:
			num := header.Number.Uint64()
			if num <= lastProcessed {
				continue
			}
			if err := processBlock(ctx, rpcClient, num, balances, balancesFile); err != nil {
				log.Warn("process block failed", "block", num, "err", err)
			}
			if err := saveProgress(stateFile, num); err != nil {
				log.Warn("save progress failed", "block", num, "err", err)
			}
			lastProcessed = num
		}
	}
}

// processBlock collects all addresses from the block and their logs, updates the balances map and writes a file
func processBlock(
	ctx context.Context,
	rpcClient *ethclient.Client,
	blockNum uint64,
	balances map[common.Address]*big.Int,
	balancesFile string,
) error {
	// load block
	blk, err := rpcClient.BlockByNumber(ctx, new(big.Int).SetUint64(blockNum))
	if err != nil {
		return err
	}
	// collect addresses from tx and receipt logs
	signer := types.LatestSignerForChainID(new(big.Int).SetUint64(1))
	addrs := make(map[common.Address]struct{})
	for _, tx := range blk.Transactions() {
		if from, err := types.Sender(signer, tx); err == nil {
			addrs[from] = struct{}{}
		}
		if to := tx.To(); to != nil {
			addrs[*to] = struct{}{}
		} else {
			from, _ := types.Sender(signer, tx)
			addrs[crypto.CreateAddress(from, tx.Nonce())] = struct{}{}
		}
		if rcpt, err := rpcClient.TransactionReceipt(ctx, tx.Hash()); err == nil {
			for _, lg := range rcpt.Logs {
				addrs[lg.Address] = struct{}{}
			}
		}
	}
	// update balances
	for addr := range addrs {
		bal, err := rpcClient.BalanceAt(ctx, addr, nil)
		if err != nil {
			log.Warn("Balance fetch failed", "addr", addr.Hex(), "err", err)
			continue
		}
		if bal.Sign() > 0 {
			balances[addr] = bal
		} else {
			delete(balances, addr)
		}
	}
	// write file with balances
	return writeBalances(balancesFile, balances)
}

// StartRabbitBalancePublisher subscribes to new blocks,
// tracks addresses with changed balance for each block
// and publishes them to the RabbitMQ queue in the format:
// [32B blockHash][20B address][1B len(balanceBytes)][N B balanceBytes]
func StartRabbitBalancePublisher(
	ctx context.Context,
	rpcClient *ethclient.Client,
	amqpURL, queueName, stateFile string,
) error {
	// --- 1) RabbitMQ ---
	conn, err := amqp.Dial(amqpURL)
	if err != nil {
		return fmt.Errorf("rabbitmq dial: %w", err)
	}
	defer conn.Close()

	ch, err := conn.Channel()
	if err != nil {
		return fmt.Errorf("rabbitmq channel: %w", err)
	}
	defer ch.Close()

	if _, err := ch.QueueDeclare(queueName, true, false, false, false, nil); err != nil {
		return fmt.Errorf("queue declare: %w", err)
	}

	// --- 2) Preparing the Eth client ---
	chainID, err := rpcClient.NetworkID(ctx)
	if err != nil {
		return fmt.Errorf("network id: %w", err)
	}
	signer := types.LatestSignerForChainID(chainID)

	// --- 3) Subscribe to new headings ---
	headers := make(chan *types.Header, 16)
	sub, err := rpcClient.SubscribeNewHead(ctx, headers)
	if err != nil {
		return fmt.Errorf("subscribe new head: %w", err)
	}
	defer sub.Unsubscribe()

	log.Info("Started real-time balance publisher")

	// --- 4) Event handling ---
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case err := <-sub.Err():
			return fmt.Errorf("subscription error: %w", err)

		case header := <-headers:
			num := header.Number.Uint64()
			if err := publishBlock(ctx, rpcClient, signer, ch, queueName, num); err != nil {
				log.Warn("publish block failed", "block", num, "err", err)
				continue
			}
			// optionally save progress
			if err := saveProgress(stateFile, num); err != nil {
				log.Warn("save progress failed", "block", num, "err", err)
			}
		}
	}
}

// publishBlock sends a non-zero balance for each address in the block
func publishBlock(
	ctx context.Context,
	rpcClient *ethclient.Client,
	signer types.Signer,
	ch *amqp.Channel,
	queueName string,
	blockNum uint64,
) error {
	blk, err := rpcClient.BlockByNumber(ctx, new(big.Int).SetUint64(blockNum))
	if err != nil {
		return fmt.Errorf("fetch block %d: %w", blockNum, err)
	}

	// Collect all addresses
	addrSet := make(map[common.Address]struct{})
	for _, tx := range blk.Transactions() {
		if from, err := types.Sender(signer, tx); err == nil {
			addrSet[from] = struct{}{}
		}
		if to := tx.To(); to != nil {
			addrSet[*to] = struct{}{}
		} else if from, err := types.Sender(signer, tx); err == nil {
			addrSet[crypto.CreateAddress(from, tx.Nonce())] = struct{}{}
		}
		if rcpt, err := rpcClient.TransactionReceipt(ctx, tx.Hash()); err == nil {
			for _, lg := range rcpt.Logs {
				addrSet[lg.Address] = struct{}{}
			}
		}
	}

	hashBytes := blk.Hash().Bytes()
	for addr := range addrSet {
		bal, err := rpcClient.BalanceAt(ctx, addr, new(big.Int).SetUint64(blockNum))
		if err != nil {
			log.Warn("balance fetch failed", "addr", addr.Hex(), "block", blockNum, "err", err)
			continue
		}
		if bal.Sign() <= 0 {
			continue
		}

		balB := bal.Bytes()
		if len(balB) > 255 {
			return fmt.Errorf("balance too large for %s at block %d", addr.Hex(), blockNum)
		}

		pkt := make([]byte, 32+20+1+len(balB))
		copy(pkt[0:32], hashBytes)
		copy(pkt[32:52], addr.Bytes())
		pkt[52] = byte(len(balB))
		copy(pkt[53:], balB)

		// 1) Log in human-readable form
		log.Info("Publishing balance",
			"block", blockNum,
			"txHash", blk.Hash().Hex(),
			"address", addr.Hex(),
			"balance", bal.String(),
		)
		// 2) If necessary - log of the full telegram (hex)
		log.Info("Raw packet", "data", hex.EncodeToString(pkt))

		if err := ch.Publish("", queueName, false, false, amqp.Publishing{
			ContentType: "application/octet-stream",
			Body:        pkt,
		}); err != nil {
			return fmt.Errorf("publish block %d addr %s: %w", blockNum, addr.Hex(), err)
		}
	}
	return nil
}
