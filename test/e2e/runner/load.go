package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gorilla/websocket"
	"strings"
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

// TODO add to toml
var window = 1

// IdTx is a Tx transaction bundled with its 16 byte identifier.
// The identifier is used to map the pending transactions to differ new commits from duplicates.
type IdTx struct {
	tx types.Tx
	id int16
}

// Load generates transactions against the network until the given context is
// canceled.
func Load(ctx context.Context, testnet *e2e.Testnet, useInternalIP bool) error {
	initialTimeout := 1 * time.Minute
	stallTimeout := 30 * time.Second
	chSuccess := make(chan struct{})
	chFailed := make(chan error)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	//generate a random byte
	c := make([]byte, 1)
	rand.Read(c)
	// This client's 1 byte id, used to make the tx ids.
	// Only one byte is used to simplify the processing during normal testing conditions but more could be used if necessary.
	clientId := c[0]

	logger.Info("load", "msg", log.NewLazySprintf("Starting transaction load (%v workers)...", workerPoolSize))
	started := time.Now()

	// windowCh is used to limit the number of pending transactions
	windowCh := make(chan struct{}, window)
	// Map the pending transactions to differentiate new commits from duplicated commits.
	pendingMap := make(map[int16]bool)
	txCh := make(chan IdTx)
	go loadGenerate(ctx, txCh, testnet, clientId)
	go monitorBlocks(ctx, windowCh, pendingMap, clientId)

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
func loadGenerate(ctx context.Context, txCh chan<- IdTx, testnet *e2e.Testnet, clientId byte) {
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
func createTxBatch(ctx context.Context, txCh chan<- IdTx, testnet *e2e.Testnet, clientId byte) {
	wg := &sync.WaitGroup{}
	genCh := make(chan struct{})
	for i := 0; i < workerPoolSize; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range genCh {
				txId, txNum := generateId(clientId)
				tx, err := payload.NewBytes(&payload.Payload{
					Id:          txId,
					Size:        uint64(testnet.LoadTxSizeBytes),
					Rate:        uint64(testnet.LoadTxBatchSize),
					Connections: uint64(testnet.LoadTxConnections),
				})
				if err != nil {
					panic(fmt.Sprintf("Failed to generate tx: %v", err))
				}

				select {
				case txCh <- IdTx{tx, txNum}:
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
func loadProcess(ctx context.Context, txCh <-chan IdTx, chSuccess chan<- struct{}, chFailed chan<- error, n *e2e.Node, useInternalIP bool, windowCh chan struct{}, pendingMap map[int16]bool) {
	var client *rpchttp.HTTP
	var err error
	s := struct{}{}
	for t := range txCh {

		tx := t.tx
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
		if _, err = client.BroadcastTxSync(ctx, tx); err != nil {
			// Free one slot on failure
			<-windowCh
			chFailed <- err
			continue
		}
		pendingMap[txNum] = true
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

/* TODO fix query and use the internal WS client
func monitorBlocks(ctx context.Context, windowCh chan struct{}, pendingMap map[int16]bool, clientId byte) {
	var subscription *rpchttp.WSEvents
	//query := `{"jsonrpc":"2.0","method":"subscribe","params":["tm.event='NewBlock'"],"id":"1"}`
	query := "'{ \"jsonrpc\": \"2.0\",\"method\": \"subscribe\",\"id\": 0,\"params\": {\"query\": \"tm.event='\"'NewBlock'\"'\"} }'"
	s, err := subscription.Subscribe(ctx, "", query, 10)
	if err != nil {
		logger.Info("error when creating a subscription", "error", err)
		return
	}

	for block := range s {
		// Time in milliseconds (as int64). More bytes than necessary but it's possibly faster than other methods in this case.
		//rcvTime := time.Now().UnixMilli()
		var txs = block.Data
		logger.Info("load", "msg", log.NewLazySprintf("---- %+v", txs))


			//txs := t.Block.data.Txs

			// List of round trip times in this block
			times := make([]int64, 0, len(txs))

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
					if pendingMap[txNum] {
						// Deletes the key to save memory.
						// Simply changing the key's value to false might be faster if this function is too slow.
						delete(pendingMap, txNum)

						// Free a slot for a new tx.
						<-windowCh

						t := rcvTime - tx.Time.AsTime().UnixMilli()
						times = append(times, t)
					}
				}

			}

			minT, avgrT, maxT, numT := calculateStatistics(times)

			logger.Info("load", "msg", log.NewLazySprintf("Block received: min latency %fs avrg latency %fs max latency %fs | %d new txs out of %d", minT, avgrT, maxT, numT, len(txs)))


	}

}*/

/*
*  TODO TEMPORARY
 */

type NewBlockSubscription struct {
	Jsonrpc string `json:"jsonrpc"`
	Id      string `json:"id"`
	Result  struct {
		Query string `json:"query"`
		Data  struct {
			Type  string `json:"type"`
			Value struct {
				Block struct {
					Header struct {
						Height string `json:"height"`
						Time   string `json:"time"`
					} `json:"header"`
					Data struct {
						Txs []string `json:"txs"`
					} `json:"data"`
				} `json:"block"`
			} `json:"value"`
		} `json:"data"`
	} `json:"result"`
}

func monitorBlocks(ctx context.Context, windowCh chan struct{}, pendingMap map[int16]bool, clientId byte) {
	subscribeRequest := `{"jsonrpc":"2.0","method":"subscribe","params":["tm.event='NewBlock'"],"id":"1"}`
	c, _, err := websocket.DefaultDialer.Dial("ws://localhost:5701/websocket", nil)
	if err != nil {
		logger.Info("error when creating a subscription", "error", err)
		return
	}
	defer c.Close()

	err = c.WriteMessage(websocket.TextMessage, []byte(subscribeRequest))
	if err != nil {
		logger.Info("error when creating a subscription", "error", err)
		return
	}

	for {
		_, message, err := c.ReadMessage()
		if err != nil {
			logger.Info("error when creating a subscription", "error", err)
			return
		}

		// Time in milliseconds (as int64). More bytes than necessary but it's possibly faster than other methods in this case.
		rcvTime := time.Now().UnixMilli()

		var NewBlock NewBlockSubscription
		err = json.Unmarshal(message, &NewBlock)
		if err != nil {
			logger.Info("error when reading a subscription", "error", err)
			continue
		}

		numTxs := len(NewBlock.Result.Data.Value.Block.Data.Txs)
		if numTxs == 0 {
			continue
		}

		// List of round trip times in this block
		times := make([]int64, 0, numTxs)

		for _, txStr := range NewBlock.Result.Data.Value.Block.Data.Txs {

			data, err := base64.StdEncoding.DecodeString(txStr)
			if err != nil {
				logger.Info("error when reading a transaction", "error", err)
				continue
			}

			tx, err := payload.FromBytes(data)

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
				if pendingMap[txNum] {
					// Deletes the key to save memory.
					// Simply changing the key's value to false might be faster if this function is too slow.
					delete(pendingMap, txNum)

					// Free a slot for a new tx.
					<-windowCh

					t := rcvTime - tx.Time.AsTime().UnixMilli()
					times = append(times, t)
				}
			}

		}

		minT, avgrT, maxT, numT := calculateStatistics(times)

		logger.Info("load", "msg", log.NewLazySprintf("Block received: min latency %fs avrg latency %fs max latency %fs | %d new txs out of %d", minT, avgrT, maxT, numT, numTxs))
	}
}
