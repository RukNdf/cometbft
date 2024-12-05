package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	rpchttp "github.com/cometbft/cometbft/rpc/client/http"
	ctypes "github.com/cometbft/cometbft/rpc/core/types"
	e2e "github.com/cometbft/cometbft/test/e2e/pkg"
	"github.com/cometbft/cometbft/test/loadtime/payload"
	"github.com/cometbft/cometbft/types"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Report runs test cases under tests.
func Report(ctx context.Context, testnet *e2e.Testnet, useInternalIP bool) error {
	outputDir := filepath.Join("networks", "logs")
	newBlockHeighQuery := "tm.event='NewBlock'"
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Create output directory if it doesn't exist
	if err := os.MkdirAll(outputDir, os.ModePerm); err != nil {
		logger.Error("error creating output directory", "error", err)
		panic(err)
	}
	logger.Info("Log directory created at:", "path", outputDir)

	outputChannels := make([]<-chan ctypes.ResultEvent, len(testnet.Nodes))
	var wg sync.WaitGroup     // Wait group to synchronize report generation goroutines
	var initWG sync.WaitGroup // Wait group for channel initialization

	// Initialize channels concurrently and wait for all to be ready
	for i, n := range testnet.Nodes {
		initWG.Add(1)
		go func(i int, n *e2e.Node) {
			defer initWG.Done()
			var client *rpchttp.HTTP
			var err error
			for {
				if useInternalIP {
					client, err = n.ClientInternalIP()
					client.WSEvents.Logger = logger
				} else {
					client, err = n.Client()
					client.WSEvents.Logger = logger
				}
				if err != nil {
					logger.Info("error creating node reporter", "node", i, "error", err)
					// Optionally, add a sleep before retrying
					continue // Retry until successful
				}
				err = client.Start()
				if err != nil {
					logger.Info("error starting client", "node", i, "error", err)
					// Optionally, add a sleep before retrying
					continue // Retry until successful
				}
				out, err := client.Subscribe(ctx, "", newBlockHeighQuery)
				if err != nil {
					logger.Info("error subscribing to node", "node", i, "error", err)
					// Optionally, add a sleep before retrying
					continue // Retry until successful
				}
				outputChannels[i] = out
				logger.Info("Channel initialized", "node", i)
				break // Exit the retry loop
			}
		}(i, n)
	}

	// Wait for all channels to be initialized
	initWG.Wait()
	logger.Info("All channels have been initialized")

	// Launch goroutines to process each channel
	for i, ch := range outputChannels {
		wg.Add(1)
		go func(index int, channel <-chan ctypes.ResultEvent) {
			defer wg.Done()
			generateReport(ctx, outputDir, channel, index)
		}(i, ch)
	}

	// Wait for all report generation goroutines to finish
	wg.Wait()

	logger.Info("All reports generated successfully")
	return nil
}

type NewBlock struct {
	Height string   `json:"height"`
	Time   string   `json:"time"`
	Txs    []string `json:"txs"`
}

func generateReport(ctx context.Context, outputDir string, ch <-chan ctypes.ResultEvent, i int) {
	logger.Info("Starting report generation", "node", i)

	txFilePath := filepath.Join(outputDir, fmt.Sprintf("txs-node%d.json", i))

	// Create and buffer the transaction log file
	//txFile, err := os.OpenFile(txFilePath, os.O_CREATE|os.O_WRONLY, 0644)
	txFile, err := os.Create(txFilePath)
	if err != nil {
		logger.Info("non-fatal error creating tx log file", "node", i, "error", err)
		return
	}
	defer txFile.Close()
	txWriter := bufio.NewWriter(txFile)
	defer txWriter.Flush()

	// Read from the channel and write to the files
	logger.Info("Waiting for messages at ", "node", i)

	for {
		select {
		case <-ctx.Done():
			logger.Info("context finalized, stopping report generation", "node", i)
			return
		case result, ok := <-ch:
			if !ok {
				logger.Info("channel closed, stopping report generation", "node", i)
				return
			}

			data := result.Data.(types.EventDataNewBlock)

			var txsAsStrings []string
			for _, txBytes := range data.Block.Data.Txs {
				b64 := base64.StdEncoding.EncodeToString(txBytes)

				str, err := url.QueryUnescape(b64)
				if err != nil {
					logger.Error("Parsing error", "error", err)
					continue
				}
				txData, err := base64.StdEncoding.DecodeString(str)
				if err != nil {
					logger.Error("Parsing error", "error", err)
					continue
				}
				out, err := payload.FromBytes(txData)
				if err != nil {
					//Prefix error means it encountered a tx that wasn't submitted by this client and thus can be ignored
					if !strings.Contains(err.Error(), "key prefix") {
						logger.Error("Parsing error", "error", err)
					}
					continue
				}
				//receivedTxs += string(out.Time.AsTime().UTC().Format(time.RFC3339Nano)) + ","

				marshal, err := json.Marshal(out)
				if err != nil {
					return
				}
				txsAsStrings = append(txsAsStrings, string(marshal))
			}

			b := &NewBlock{
				Height: strconv.FormatInt(data.Block.Height, 10),
				Time:   data.Block.Time.String(),
				Txs:    txsAsStrings,
			}

			jsonString, err := json.Marshal(b)
			if err != nil {
				return
			}
			// Simulate transaction logging (you can modify this as needed)
			_, err = txWriter.WriteString(fmt.Sprintf(string(jsonString) + "\n"))
			if err != nil {
				logger.Info("error writing to tx log file", "node", i, "error", err)
			}
		}
	}
}
