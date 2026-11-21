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

type cpuTicks struct {
	total   uint64
	idleAll uint64
}

// CountCores inspects /proc/stat in procDir to count detected CPU cores ("cpu0", "cpu1", ...).
// If /proc/stat is unreadable or has no individual cores, it returns fallback.
func CountCores(procDir string, fallback int) int {
	ticks, err := readCPUTicks(procDir)
	if err != nil || len(ticks) == 0 {
		return fallback
	}
	count := 0
	for k := range ticks {
		if k != "cpu" && strings.HasPrefix(k, "cpu") {
			count++
		}
	}
	if count > 0 {
		return count
	}
	return fallback
}
func readCPUTicks(procDir string) (map[string]cpuTicks, error) {
	path := filepath.Join(procDir, "stat")
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open stat: %w", err)
	}
	defer f.Close()

	res := make(map[string]cpuTicks)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "cpu") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		name := fields[0]
		if name != "cpu" && !strings.HasPrefix(name, "cpu") {
			continue
		}

		// user, nice, system, idle, iowait, irq, softirq, steal, guest, guest_nice
		var user, nice, system, idle, iowait, irq, softirq, steal uint64
		if len(fields) > 1 {
			user, _ = strconv.ParseUint(fields[1], 10, 64)
		}
		if len(fields) > 2 {
			nice, _ = strconv.ParseUint(fields[2], 10, 64)
		}
		if len(fields) > 3 {
			system, _ = strconv.ParseUint(fields[3], 10, 64)
		}
		if len(fields) > 4 {
			idle, _ = strconv.ParseUint(fields[4], 10, 64)
		}
		if len(fields) > 5 {
			iowait, _ = strconv.ParseUint(fields[5], 10, 64)
		}
		if len(fields) > 6 {
			irq, _ = strconv.ParseUint(fields[6], 10, 64)
		}
		if len(fields) > 7 {
			softirq, _ = strconv.ParseUint(fields[7], 10, 64)
		}
		if len(fields) > 8 {
			steal, _ = strconv.ParseUint(fields[8], 10, 64)
		}

		total := user + nice + system + idle + iowait + irq + softirq + steal
		idleAll := idle + iowait
		res[name] = cpuTicks{total: total, idleAll: idleAll}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan stat: %w", err)
	}
	return res, nil
}

// computeCPUPercentage calculates utilization percentage between two tick readings.
func computeCPUPercentage(prev, curr cpuTicks) float64 {
	if curr.total <= prev.total {
		return 0.0
	}
	deltaTotal := curr.total - prev.total
	deltaIdle := curr.idleAll - prev.idleAll
	if deltaTotal <= deltaIdle {
		return 0.0
	}
	pct := float64(deltaTotal-deltaIdle) / float64(deltaTotal) * 100.0
	if pct < 0.0 {
		return 0.0
	}
	if pct > 100.0 {
		return 100.0
	}
	return pct
}

// readLoadAvg parses /proc/loadavg and returns 1m, 5m, and 15m load averages.
func readLoadAvg(procDir string, now int64) ([]model.MetricSample, error) {
	path := filepath.Join(procDir, "loadavg")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read loadavg: %w", err)
	}

	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return nil, fmt.Errorf("unexpected loadavg format: %q", string(data))
	}

	l1, err1 := strconv.ParseFloat(fields[0], 64)
	l5, err2 := strconv.ParseFloat(fields[1], 64)
	l15, err3 := strconv.ParseFloat(fields[2], 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return nil, fmt.Errorf("parse loadavg values: %v, %v, %v", err1, err2, err3)
	}

	return []model.MetricSample{
		{Entity: "host", Metric: "load_1m", TS: now, Value: l1},
		{Entity: "host", Metric: "load_5m", TS: now, Value: l5},
		{Entity: "host", Metric: "load_15m", TS: now, Value: l15},
	}, nil
}
