package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"gonum.org/v1/plot"
	"gonum.org/v1/plot/plotter"
	"gonum.org/v1/plot/vg"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func PlotBlock() error {
	// Abra o arquivo (substituir "blocks.json" pelo seu arquivo)
	outputDir := filepath.Join("networks", "logs")
	blkFilePath := filepath.Join(outputDir, fmt.Sprintf("blk-node%d.json", 0))
	f, err := os.Open(blkFilePath)
	if err != nil {
		logger.Error("Erro ao abrir arquivo: %v", err)
		return err
	}
	defer f.Close()

	// Vamos coletar (x, y) onde:
	//   x = tempo em segundos (baseado no bloco inicial)
	//   y = número de transações (campo "size")
	//var data plotter.XYs
	var firstTime time.Time
	var foundFirstTime bool
	// dataLine: (x, y) para plot de linha
	// valsHist: armazena somente os valores de "size" para o histograma
	var dataLine plotter.XYs
	var valsHist plotter.Values

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		var block NewBlock
		if err := json.Unmarshal([]byte(line), &block); err != nil {
			logger.Error("Erro ao unmarshal JSON: %v - Linha: %s", err, line)
			continue
		}

		// (Opcional) Se você quiser transformar block.Time (string) em time.Time,
		// pode tentar parse manualmente, pois o formato está "2024-12-27 15:41:21.842269372 +0000 UTC"
		// Exemplo:
		parsedTime, parseErr := time.Parse("2006-01-02 15:04:05.999999999 -0700 MST", block.Time)
		if parseErr == nil {
			// Se conseguiu parsear, sobrescreve em block.Time ou armazena separado
			block.Time = parsedTime.Format(time.RFC3339Nano)
		} else {
			// Caso não queira logar erro, pode ignorar
			// log.Printf("Não foi possível parsear a hora em time.Time: %v", parseErr)
		}

		// Precisamos transformar o parsedTime em algum float64
		// Exemplo: segs desde o primeiro bloco
		if !foundFirstTime {
			firstTime = parsedTime
			foundFirstTime = true
		}
		delta := parsedTime.Sub(firstTime).Seconds()

		// (x, y) = (delta, block.Size)
		// mas se block.Size for o número de transações
		//data = append(data, plotter.XY{X: delta, Y: float64(block.Size)})

		// Somente adiciona nos gráficos se size > 0
		if block.Size > 0 {
			// Para o gráfico de linha: (x=delta, y=Size)
			dataLine = append(dataLine, plotter.XY{X: delta, Y: float64(block.Size)})

			// Para o histograma: só queremos o size
			valsHist = append(valsHist, float64(block.Size))
		}

		// Agora você tem block com os dados da linha
		fmt.Printf("Block: Height=%s, Size=%d, Time=%s, Round=%d, Elapsed=%.6f\n",
			block.Height, block.Size, block.Time, block.Round, block.ElapsedSinceLastBlock)
	}

	if err := scanner.Err(); err != nil {
		logger.Error("Erro durante a leitura do arquivo: %v", err)
	}

	// ====== GRÁFICO DE LINHA ======
	pLine := plot.New()
	pLine.Title.Text = "Transações (size>0) por Bloco no Tempo"
	pLine.X.Label.Text = "Segundos desde o 1º bloco"
	pLine.Y.Label.Text = "Nº de Transações (size)"
	pLine.X.Min = 0
	pLine.Y.Min = 0

	if len(dataLine) > 0 {
		linePlot, err := plotter.NewLine(dataLine)
		if err != nil {
			logger.Error("Erro criando Line", "err", err)
		} else {
			pLine.Add(linePlot)
		}
	}

	if err := pLine.Save(8*vg.Inch, 4*vg.Inch, "transacoes_line.png"); err != nil {
		logger.Error("Erro ao salvar transacoes_line.png", "err", err)
	} else {
		fmt.Println("Gráfico de linha gerado em transacoes_line.png")
	}

	// ====== HISTOGRAMA + CDF ======
	if len(valsHist) == 0 {
		logger.Error("Nenhum bloco com size>0 para histograma/CDF.")
		return nil
	}

	pHist := plot.New()
	pHist.Title.Text = "Histograma Txs por Bloco (>0) + CDF"
	pHist.X.Label.Text = "Txs por Bloco"
	pHist.Y.Label.Text = "Frequência (Hist) / Contagem (CDF)"
	pHist.X.Min = 0
	pHist.Y.Min = 0

	// 1) Hist bins of width=25
	maxVal := 0.0
	for _, v := range valsHist {
		if v > maxVal {
			maxVal = v
		}
	}
	binWidth := 25.0
	numBins := int(math.Ceil(maxVal / binWidth))

	hist, err := plotter.NewHist(valsHist, numBins)
	if err != nil {
		logger.Error("Erro ao criar hist", "err", err)
		return err
	}
	hist.Width = binWidth

	pHist.Add(hist)

	// 2) CDF
	// Sort the values
	sortedVals := make([]float64, len(valsHist))
	copy(sortedVals, valsHist)
	sort.Float64s(sortedVals)

	// Build a XY for the CDF
	cdfXY := make(plotter.XYs, len(sortedVals))
	n := float64(len(sortedVals))
	for i, v := range sortedVals {
		cdfXY[i].X = v
		cdfXY[i].Y = float64(i + 1) // cumul count (1..n)
	}

	cdfLine, err := plotter.NewLine(cdfXY)
	if err != nil {
		logger.Error("Erro criando cdf line", "err", err)
		return err
	}
	// optionally set color, style, etc.
	// cdfLine.Color = color.RGBA{G: 255, A: 255}

	pHist.Add(cdfLine)
	maxCount := 0.0
	for _, bin := range hist.Bins {
		if bin.Max > maxCount {
			maxCount = bin.Max
		}
	}
	// Ajustamos o eixo Y para exibir ou o count máximo ou n (se quiser sobrepor uma CDF que chega até n)
	pHist.Y.Max = math.Max(maxCount, n)
	// Ajustar pHist.Y.Max para ver todo o cdf
	// O CDF chega a n (qtd total). O hist max é hist.Data().MaxCount
	maxY := math.Max(maxCount, n)
	pHist.Y.Max = maxY

	// Salvar
	if err := pHist.Save(8*vg.Inch, 4*vg.Inch, "transacoes_hist_cdf.png"); err != nil {
		logger.Error("Erro ao salvar hist+cdf", "err", err)
	} else {
		fmt.Println("Histograma + CDF gerado em transacoes_hist_cdf.png")
	}

	return nil
}

// This function is appended to handle the new file's data
func PlotLatencyDistribution() error {
	// Suppose the file with latencies is "transactions-latency.json"
	// or any other. Adjust as needed:
	latencyFile := filepath.Join("networks", "logs", "txs-node0.json")
	f, err := os.Open(latencyFile)
	if err != nil {
		logger.Error("Erro ao abrir arquivo de latência", "err", err)
		return err
	}
	defer f.Close()

	var latencies []float64

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var txLat NewTx
		if err := json.Unmarshal([]byte(line), &txLat); err != nil {
			logger.Error("Erro unmarshaling tx latency JSON", "err", err, "line", line)
			continue
		}
		// Convert latency from ns to seconds
		latSec := float64(txLat.Latency) / 1e9
		latencies = append(latencies, latSec)
	}
	if err := scanner.Err(); err != nil {
		logger.Error("Erro lendo arquivo de latência", "err", err)
	}

	if len(latencies) == 0 {
		logger.Error("Nenhum dado de latência para plotar.")
		return nil
	}

	// Build a new plot
	p := plot.New()
	p.Title.Text = "Histograma de Latência + CDF"
	p.X.Label.Text = "Latência (segundos)"
	p.Y.Label.Text = "Frequência / Contagem"

	// Hist
	maxVal := 0.0
	for _, v := range latencies {
		if v > maxVal {
			maxVal = v
		}
	}
	// pick a binWidth or any approach
	binWidth := maxVal / 15.0 // or some other logic
	if binWidth < 1e-9 {
		// Avoid zero or extremely small bins if maxVal is very small
		binWidth = 1e-9
	}

	valsHist := plotter.Values(latencies)
	numBins := int(math.Ceil(maxVal / binWidth))
	if numBins <= 0 {
		numBins = 1
	}

	hist, err := plotter.NewHist(valsHist, numBins)
	if err != nil {
		logger.Error("Erro criando hist de latência", "err", err)
		return err
	}
	hist.Width = binWidth
	p.Add(hist)

	// CDF
	sortedLat := make([]float64, len(latencies))
	copy(sortedLat, latencies)
	sort.Float64s(sortedLat)

	cdfXY := make(plotter.XYs, len(sortedLat))
	n := float64(len(sortedLat))
	for i, v := range sortedLat {
		// X = v seconds, Y = i+1 (cumulative count)
		cdfXY[i].X = v
		cdfXY[i].Y = float64(i + 1)
	}
	cdfLine, err := plotter.NewLine(cdfXY)
	if err != nil {
		logger.Error("Erro criando cdf line (lat)", "err", err)
		return err
	}
	p.Add(cdfLine)

	// find maxCount for histogram
	var histMax float64
	for _, bin := range hist.Bins {
		if bin.Max > histMax {
			histMax = bin.Max
		}
	}
	// Eixo Y precisa acomodar o hist e a CDF (que pode chegar a n)
	p.Y.Max = math.Max(histMax, n)

	// Save
	outName := "latency_hist_cdf.png"
	if err := p.Save(8*vg.Inch, 4*vg.Inch, outName); err != nil {
		logger.Error("Erro ao salvar hist+cdf latência", "err", err)
	} else {
		fmt.Printf("Histograma + CDF de latência gerado em %s\n", outName)
	}

	return nil
}

// PlotLatencyByLoadPeriod looks for the largest period with a certain load and calculates the average latency for each period.
// If the latency is a flat line it shows that the system can deal with the increase in load,
// if the latency increases that means the system can no longer deal with the load and is taking more and more to process it.
func PlotLatencyByLoadPeriod() error {
	// Minimum sequence blocks to include in the graph.
	// Smaller sequences are disregarded as noise.
	const blockPeriodThreshold = 3

	// Average latency and the number of items that generated that average
	type avgLat struct {
		lat float64
		num int
	}

	// Suppose the file with latencies is "transactions-latency.json"
	// or any other. Adjust as needed:
	latencyFile := filepath.Join("networks", "logs", "txs-node0.json")
	f, err := os.Open(latencyFile)
	if err != nil {
		logger.Error("Erro ao abrir arquivo de latência", "err", err)
		return err
	}
	defer f.Close()

	// Latencies records the average latency of each block and numTx records the number of transactions in that same block.
	// It assumes the transactions were written sequentially which is true for the current report module.
	var latencies []avgLat

	curLatSum := 0.0
	curNumTx := 0
	currentHeight := ""

	// Read file and compile data
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var tx NewTx
		if err := json.Unmarshal([]byte(line), &tx); err != nil {
			logger.Error("Erro unmarshaling tx latency JSON", "err", err, "line", line)
			continue
		}

		// Another block started, record information of the current block and reset counters
		if tx.Height != currentHeight {
			if currentHeight != "" {
				average := curLatSum / float64(curNumTx)
				latencies = append(latencies, avgLat{average, curNumTx})
			}
			currentHeight = tx.Height
			curLatSum = 0
			curNumTx = 0
		}

		// Convert latency from ns to seconds and add to counter
		curLatSum += float64(tx.Latency) / 1e9
		curNumTx++
	}
	//record last block
	if currentHeight != "" {
		average := curLatSum / float64(curNumTx)
		latencies = append(latencies, avgLat{average, curNumTx})
	}
	if err := scanner.Err(); err != nil {
		logger.Error("Erro lendo arquivo de latência", "err", err)
	}

	if len(latencies) == 0 {
		logger.Error("Nenhum dado de latência para plotar.")
		return nil
	}

	// Find the longest period for each block size
	averageLat := make(map[int]avgLat)
	curBlockSize := 0
	curPeriodLen := 0
	curLatSum = 0
	for _, v := range latencies {
		n := v.num
		l := v.lat
		// Block size changed, write previous to map if it is over the threshold
		if n != curBlockSize {
			if curPeriodLen > blockPeriodThreshold {
				if curPeriodLen > averageLat[curBlockSize].num {
					average := curLatSum / float64(curPeriodLen)
					averageLat[curBlockSize] = avgLat{average, curPeriodLen}
				}
			}
			curLatSum = 0
			curPeriodLen = 0
			curBlockSize = n
		}
		curLatSum += l
		curPeriodLen++
	}
	// Process last block if it went over the threshold as it won't be caught by the loop
	if curPeriodLen > blockPeriodThreshold {
		if curPeriodLen > averageLat[curBlockSize].num {
			average := curLatSum / float64(curPeriodLen)
			averageLat[curBlockSize] = avgLat{average, curPeriodLen}
		}
	}

	if len(averageLat) == 0 {
		logger.Error("Nenhum período de latência para plotar.")
		return nil
	}

	// Build a new plot
	p := plot.New()
	p.Title.Text = "Latência média por carga"
	p.X.Label.Text = "Transações por bloco"
	p.Y.Label.Text = "Latência (segundos)"

	// Get borders
	maxX := 0
	minX := math.MaxInt
	minY := 0.0 // Y is the time axis. We can leave it as 0 since blocks rarely take more than a few seconds.
	maxY := 0.0

	var points plotter.XYs
	for blockSize := range averageLat {
		lat := averageLat[blockSize].lat
		points = append(points, plotter.XY{X: float64(blockSize), Y: lat})
		//update values
		if blockSize > maxX {
			maxX = blockSize
		}
		if blockSize < minX {
			minX = blockSize
		}
		if lat > maxY {
			maxY = lat
		}
	}

	scatter, err := plotter.NewScatter(points)
	if err != nil {
		logger.Error("Erro criando scatter de latência por período", "err", err)
		return err
	}
	p.Add(scatter)

	p.X.Max = float64(maxX)
	p.X.Min = float64(minX)
	p.Y.Max = maxY
	p.Y.Min = minY

	// Save
	outName := "latency_period_scatter.png"
	if err := p.Save(8*vg.Inch, 4*vg.Inch, outName); err != nil {
		logger.Error("Erro ao salvar scatter de latência por período", "err", err)
	} else {
		fmt.Printf("Scatter de latência por período gerado em %s\n", outName)
	}

	return nil
}

// The main function now calls both the existing Plotter and the new PlotLatencyDistribution
func Plotter() error {
	if err := PlotBlock(); err != nil {
		logger.Error("err", err)
		return err
	}
	if err := PlotLatencyDistribution(); err != nil {
		logger.Error("err", err)
		return err
	}
	if err := PlotLatencyByLoadPeriod(); err != nil {
		logger.Error("err", err)
		return err
	}
	return nil
}
