package host

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dale-tomson/Tarsier/internal/model"
)

// readMemoryMetrics parses /proc/meminfo and returns host RAM and swap metrics in bytes and percentage.
func readMemoryMetrics(procDir string, now int64) ([]model.MetricSample, error) {
	path := filepath.Join(procDir, "meminfo")
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open meminfo: %w", err)
	}
	defer f.Close()

	var (
		memTotal     uint64
		memFree      uint64
		memAvailable uint64
		buffers      uint64
		cached       uint64
		sReclaimable uint64
		swapTotal    uint64
		swapFree     uint64
		hasAvailable bool
	)

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		idx := strings.IndexByte(line, ':')
		if idx == -1 {
			continue
		}
		key := line[:idx]
		rest := strings.TrimSpace(line[idx+1:])
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		valKB, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		valBytes := valKB * 1024

		switch key {
		case "MemTotal":
			memTotal = valBytes
		case "MemFree":
			memFree = valBytes
		case "MemAvailable":
			memAvailable = valBytes
			hasAvailable = true
		case "Buffers":
			buffers = valBytes
		case "Cached":
			cached = valBytes
		case "SReclaimable":
			sReclaimable = valBytes
		case "SwapTotal":
			swapTotal = valBytes
		case "SwapFree":
			swapFree = valBytes
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan meminfo: %w", err)
	}

	if !hasAvailable {
		memAvailable = memFree + buffers + cached + sReclaimable
	}
	if memAvailable > memTotal {
		memAvailable = memTotal
	}

	var memUsed uint64
	if memTotal >= memAvailable {
		memUsed = memTotal - memAvailable
	}

	var memUsedPct float64
	if memTotal > 0 {
		memUsedPct = (float64(memUsed) / float64(memTotal)) * 100.0
	}

	memCached := cached + buffers + sReclaimable

	var swapUsed uint64
	if swapTotal >= swapFree {
		swapUsed = swapTotal - swapFree
	}

	samples := []model.MetricSample{
		{Entity: "host", Metric: "mem_total", TS: now, Value: float64(memTotal)},
		{Entity: "host", Metric: "mem_available", TS: now, Value: float64(memAvailable)},
		{Entity: "host", Metric: "mem_used", TS: now, Value: float64(memUsed)},
		{Entity: "host", Metric: "mem_used_pct", TS: now, Value: memUsedPct},
		{Entity: "host", Metric: "mem_cached", TS: now, Value: float64(memCached)},
		{Entity: "host", Metric: "swap_total", TS: now, Value: float64(swapTotal)},
		{Entity: "host", Metric: "swap_used", TS: now, Value: float64(swapUsed)},
	}

	return samples, nil
}
