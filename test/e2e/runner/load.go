package main

import (
	"bufio"
	"context"
	"google.golang.org/protobuf/types/known/timestamppb"
	"os"
	"path/filepath"
	"strings"

	//"encoding/base64"
	//"encoding/json"
	"errors"
	"fmt"
	//"github.com/gorilla/websocket"
	//"strings"
	"sync"
	"time"

	"crypto/rand"
	"github.com/cometbft/cometbft/libs/log"
	rpchttp "github.com/cometbft/cometbft/rpc/client/http"
	e2e "github.com/cometbft/cometbft/test/e2e/pkg"
	"github.com/cometbft/cometbft/test/loadtime/payload"
	"github.com/cometbft/cometbft/types"
)

const workerPoolSize = 16

// Monitor client connections vars
const monitorClientAttempts = 10
const monitorClientATime = 250

// IdPayload is a payload object bundled with its 2 byte identifier in int16 form.
// The identifier is used to map the pending transactions to differ new commits from duplicates.
type IdPayload struct {
	p  payload.Payload
	id int16
}

// Load generates transactions against the network until the given context is
// canceled.
func Load(ctx context.Context, testnet *e2e.Testnet, useInternalIP bool) error {

	logger.Info(fmt.Sprintf("Batch %v", testnet.LoadTxBatchSize))
	initialTimeout := 1 * time.Minute
	stallTimeout := 30 * time.Second
	chSuccess := make(chan struct{})
	chFailed := make(chan error)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	//generate a random byte
	c := make([]byte, 1)
	_, err := rand.Read(c)
	if err != nil {
		panic(fmt.Sprintf("Failed to generate client byte: %v", err))
	}
	// This client's 1 byte id, used to make the tx ids.
	// Only one byte is used to simplify the processing during normal testing conditions but more could be used if necessary.
	clientId := c[0]

	logger.Info("load", "msg", log.NewLazySprintf("Starting transaction load (%v workers)...", workerPoolSize))
	started := time.Now()

	// windowCh is used to limit the number of pending transactions
	windowCh := make(chan struct{}, testnet.LoadTxWindowSize)
	// Map the pending transactions to differentiate new commits from duplicated commits.
	pendingMap := new(sync.Map)
	// channel between the tx generator and the client function, with buffer size of 1.5x the window
	txCh := make(chan IdPayload, int((3*testnet.LoadTxWindowSize)/2))
	// Signals to stop the window report thread.
	stopReport := make(chan struct{})
	windowConfirm := make(chan struct{})
	// Reports the number of new transactions in a block to calculate how many are still pending
	newTxCh := make(chan int)
	go windowReporter(stopReport, windowConfirm, windowCh, txCh, testnet.LoadTxWindowSize, testnet.LoadWindowReportTime, newTxCh)
	defer stopWindowReport(stopReport, windowConfirm)
	go loadGenerate(ctx, txCh, testnet, clientId)
	// monitorBlocks is currently hardcoded to attach to the first node. Maybe changing it to a toml variable would be better.
	go monitorBlocks(ctx, windowCh, pendingMap, clientId, testnet.Nodes[0], useInternalIP, newTxCh)

	for _, n := range testnet.Nodes {
		if n.SendNoLoad {
			continue
		}

		for w := 0; w < testnet.LoadTxConnections; w++ {
			go loadProcess(ctx, txCh, chSuccess, chFailed, n, useInternalIP, windowCh, pendingMap)
		}
	}

	// Monitor successful and failed transactions, and abort on stalls.
	success, failed := 0, 0
	errorCounter := make(map[string]int)
	timeout := initialTimeout
	for {
		rate := log.NewLazySprintf("%.1f", float64(success)/time.Since(started).Seconds())

		select {
		case <-chSuccess:
			success++
			timeout = stallTimeout
		case err := <-chFailed:
			failed++
			errorCounter[err.Error()]++
		case <-time.After(timeout):
			return fmt.Errorf("unable to submit transactions for %v", timeout)
		case <-ctx.Done():
			if success == 0 {
				return errors.New("failed to submit any transactions")
			}
			logger.Info("load", "msg", log.NewLazySprintf("Ending transaction load after %v txs (%v tx/s)...", success, rate))
			return nil
		}

		// Log every ~1 second the number of sent transactions.
		total := success + failed
		if total%testnet.LoadTxBatchSize == 0 {
			successRate := float64(success) / float64(total)
			logger.Debug("load", "success", success, "failed", failed, "success/total", log.NewLazySprintf("%.2f", successRate), "tx/s", rate)
			if len(errorCounter) > 0 {
				for err, c := range errorCounter {
					if c == 1 {
						logger.Error("failed to send transaction", "err", err)
					} else {
						logger.Error("failed to send multiple transactions", "count", c, "err", err)
					}
				}
				errorCounter = make(map[string]int)
			}
		}

		// Check if reached max number of allowed transactions to send.
		if testnet.LoadMaxTxs > 0 && success >= testnet.LoadMaxTxs {
			logger.Info("load", "msg", log.NewLazySprintf("Ending transaction load after reaching %v txs (%v tx/s)...", success, rate))
			return nil
		}
	}
}

// loadGenerate generates jobs until the context is canceled.
func loadGenerate(ctx context.Context, txCh chan<- IdPayload, testnet *e2e.Testnet, clientId byte) {
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-t.C:
		case <-ctx.Done():
			close(txCh)
			return
		}
		t.Reset(time.Second)

		// A context with a timeout is created here to time the createTxBatch
		// function out. If createTxBatch has not completed its work by the time
		// the next batch is set to be sent out, then the context is canceled so that
		// the current batch is halted, allowing the next batch to begin.
		tctx, cf := context.WithTimeout(ctx, time.Second)
		createTxBatch(tctx, txCh, testnet, clientId)
		cf()
	}
}

// Transaction id.
// 2 bytes is enough for 65k transactions but more could be added if necessary.
var loadIDCounter = int16(0)
var loadIDSemaphore = make(chan struct{}, 1)

// Generates an ID for the transaction.
// Load_window uses 1 byte to identify the client, 2 bytes for the transaction.
func generateId(clientId byte) ([]byte, int16) {
	// Block channel as the tx generator is multithreaded
	loadIDSemaphore <- struct{}{}
	txNum := loadIDCounter
	loadIDCounter++
	<-loadIDSemaphore

	// Generate an ID in the format client - tx num.
	// Tx num uses two bytes to allow for more transactions without an overflow.
	id := []byte{clientId, byte(txNum >> 8), byte(txNum)}

	return id, txNum
}

// createTxBatch creates new transactions and sends them into the txCh. createTxBatch
// returns when either a full batch has been sent to the txCh or the context
// is canceled.
func createTxBatch(ctx context.Context, txCh chan<- IdPayload, testnet *e2e.Testnet, clientId byte) {
	wg := &sync.WaitGroup{}
	genCh := make(chan struct{})
	for i := 0; i < workerPoolSize; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range genCh {
				txId, txNum := generateId(clientId)
				p := payload.Payload{
					Id:          txId,
					Size:        uint64(testnet.LoadTxSizeBytes),
					Rate:        uint64(testnet.LoadTxBatchSize),
					Connections: uint64(testnet.LoadTxConnections),
				}

				select {
				case txCh <- IdPayload{p, txNum}:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	for i := 0; i < testnet.LoadTxBatchSize; i++ {
		select {
		case genCh <- struct{}{}:
		case <-ctx.Done():
		}
	}
	close(genCh)
	wg.Wait()
}

// loadProcess processes transactions by sending transactions received on the txCh
// to the client.
func loadProcess(ctx context.Context, txCh <-chan IdPayload, chSuccess chan<- struct{}, chFailed chan<- error, n *e2e.Node, useInternalIP bool, windowCh chan struct{}, pendingMap *sync.Map) {
	var client *rpchttp.HTTP
	var err error
	s := struct{}{}
	for t := range txCh {

		p := t.p
		txNum := t.id
		if client == nil {
			if useInternalIP {
				client, err = n.ClientInternalIP()
			} else {
				client, err = n.Client()
			}
			if err != nil {
				logger.Info("non-fatal error creating node client", "error", err)
				continue
			}
		}
		// Take one window slot per transaction, block if none is available
		windowCh <- struct{}{}
		// update time,
		p.Time = timestamppb.Now()
		// encode,
		tx, err := payload.NewBytes(&p)
		if err != nil {
			<-windowCh
			logger.Info("Failed to generate tx:", "error", err)
			continue
		}
		// and send
		if _, err = client.BroadcastTxSync(ctx, tx); err != nil {
			// Free one slot on failure
			<-windowCh
			chFailed <- err
			continue
		}
		pendingMap.Store(txNum, true)
		chSuccess <- s
	}
}

func milliToSeconds(m int) float32 {
	return float32(m) / 1000
}

// calculateStatistics receives a list of time differences (in milliseconds) and returns a minimum, average, and maximum (in seconds) and the number of transactions.
func calculateStatistics(times []int64) (float32, float32, float32, int) {
	num := len(times)
	// No transactions
	if num == 0 {
		return 0, 0, 0, 0
	}

	sum := 0
	minT := int(times[0])
	maxT := minT
	for _, t64 := range times {
		t := int(t64)
		sum += t
		if t < minT {
			minT = t
		} else {
			if t > maxT {
				maxT = t
			}
		}
	}

	return milliToSeconds(minT), milliToSeconds(sum) / float32(num), milliToSeconds(maxT), num
}

func createClient(n *e2e.Node, useInternalIP bool) *rpchttp.HTTP {
	var client *rpchttp.HTTP
	var err error
	if useInternalIP {
		client, err = n.ClientInternalIP()
	} else {
		client, err = n.Client()
	}
	if err != nil {
		logger.Info("error creating subscription client", "error", err)
		return nil
	}
	sleepTime := time.Duration(monitorClientATime) * time.Millisecond
	for i := 0; i < monitorClientAttempts; i++ {
		err = client.Start()
		if err != nil {
			logger.Info("error starting subscription client service", "error", err)
			if i == monitorClientAttempts-1 {
				panic(fmt.Sprintf("Max attempts reached when trying to start subscription service: %v", err))
				return nil
			}
			time.Sleep(sleepTime)
			continue
		}
		return client
	}
	return nil
}

// monitorBlocks uses a ws subscription to receive new blocks and verify which transactions have been commited.
// As new transactions come the pending window gets updated and the latency of the transactions is calculated.
func monitorBlocks(ctx context.Context, windowCh chan struct{}, pendingMap *sync.Map, clientId byte, node *e2e.Node, useInternalIP bool, newTxCh chan int) {
	client := createClient(node, useInternalIP)
	//Create a subscription channel from the client to receive new blocks
	query := `tm.event='NewBlock'`
	s, err := client.Subscribe(ctx, "", query, 10)
	if err != nil {
		logger.Info("error when creating a subscription", "error", err)
		return
	}

	for {
		r := <-s
		// Time in milliseconds (as int64). More bytes than necessary but it's possibly faster than other methods in this case.
		rcvTime := time.Now().UnixMilli()

		// Extract block from reply
		var data = r.Data
		b, newBlock := data.(types.EventDataNewBlock)
		if !newBlock {
			continue
		}
		txs := b.Block.Txs

		// List of round trip times in this block
		times := make([]int64, 0, len(txs))

		// Iterate through each transaction, checking if it was sent from this client and is not duplicated.
		for _, tx := range txs {
			tx, err := payload.FromBytes(tx)
			if err != nil {
				// Prefix error means it encountered a tx that wasn't submitted by this client and thus can be ignored
				if !strings.Contains(err.Error(), "key prefix") {
					logger.Info("error when reading a transaction", "error", err)
				}
				continue
			}

			id := tx.Id
			// Ignore transactions from other clients
			if id[0] == clientId {
				var txNum int16 = (int16(id[1]) << 8) + int16(id[2])
				// Ignore non-pending transactions (e.g. duplicates)
				// CompareAndDelete removes the key from the map to save memory
				// and only returns true if txNum was a pending transaction.
				if pendingMap.CompareAndDelete(txNum, true) {
					// Free a slot for a new tx.
					<-windowCh

					t := rcvTime - tx.Time.AsTime().UnixMilli()
					times = append(times, t)
				}
			}

		}

		minT, avrgT, maxT, numT := calculateStatistics(times)
		newTxCh <- numT
		logger.Info("load", "msg", log.NewLazySprintf("Block received: min latency %fs avrg latency %fs max latency %fs | %d new txs out of %d", minT, avrgT, maxT, numT, len(txs)))

		//Exit when done
		select {
		case <-ctx.Done():
			return
		default:
		}
	}

}

// windowReporter logs the current state of the application to the window-report.txt file.
// Each line has a timestamp, the current state of the window channel, and the current state of the tx channel.
// Its expected that both channels will "always" be full.
// Any fewer in the window means that either the client can't keep up
// or, if the number of transactions is too big, that the network or the mempool can't handle the load.
// A non-full tx channel means either the generator isn't keeping up or the load_tx_batch_size variable is too small.
func windowReporter(stopReport chan struct{}, reportConfirm chan struct{}, windowCh chan struct{}, txCh chan IdPayload, windowSize int, windowReportTimer int, newTxCh chan int) {
	logFolder := filepath.Join("networks", "logs")
	// Create output directory if it doesn't exist
	if err := os.MkdirAll(logFolder, os.ModePerm); err != nil {
		logger.Error("error creating output directory", "error", err)
		panic(err)
	}

	// Create log file
	logger.Info("Starting window report generation")
	txFilePath := filepath.Join(logFolder, "window-log.txt")
	txFile, err := os.Create(txFilePath)
	if err != nil {
		logger.Info("Error creating window log file", "error", err)
		return
	}
	defer txFile.Close()
	txWriter := bufio.NewWriter(txFile)
	defer txWriter.Flush()

	// Constant
	txSize := cap(txCh)
	reportTimer := time.Duration(windowReportTimer) * time.Millisecond

	// Keep track of pending transactions
	pending := 0
	// Count how many times there were multiple transactions pending, indicating a possible timeout
	pendingCount := 0
	const timeoutPending = 15

	// Periodically pool data and write to log file
	for {
		// Wait for the shutdown signal or for the timer to end.
		// time.After is necessary as time.Sleep can't be interrupted while it's sleeping.
		select {
		case <-stopReport:
			reportConfirm <- struct{}{}
			return
		case newTx := <-newTxCh:
			t := time.Now().UTC()
			pending -= newTx
			if pending < 0 {
				pending = 0
			}
			_, err = txWriter.WriteString(fmt.Sprintf("%s - monitor received %d new transactions. %d still pending.\n", t, newTx, pending))
			if err != nil {
				logger.Info("error writing to window log file", "error", err)
			}
			if pending > 0 {
				pendingCount++
				if pendingCount > timeoutPending {
					_, err = txWriter.WriteString(fmt.Sprintf("WARNING multiple transactions pending for %d blocks, some transactions may have timed out.", pendingCount))
					if err != nil {
						logger.Info("error writing to window log file", "error", err)
					}
				}
			}
		case <-time.After(reportTimer):
			t := time.Now().UTC()
			pending = len(windowCh)
			txLen := len(txCh)
			_, err = txWriter.WriteString(fmt.Sprintf("%s - %d out of %d window slots used. %d transactions generated out of %d.\n", t, pending, windowSize, txLen, txSize))
			if err != nil {
				logger.Info("error writing to window log file", "error", err)
			}
		}
	}
}

// tries to stop windowReport and awaits confirmation
func stopWindowReport(stopReport chan struct{}, reportConfirm chan struct{}) {
	stopReport <- struct{}{}
	<-reportConfirm
}
