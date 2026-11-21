package docker

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dale-tomson/Tarsier/internal/model"
)

// MetricSink defines the interface for emitting metrics to ingestors or caches.
type MetricSink interface {
	Push(samples ...model.MetricSample) int
}

type ioCounterState struct {
	TS       int64
	RxBytes  uint64
	TxBytes  uint64
	BlkRead  uint64
	BlkWrite uint64
}

// ContainerMetrics holds calculated point-in-time telemetry metrics for a container.
type ContainerMetrics struct {
	CPUPct      float64
	MemUsed     float64
	MemLimit    float64
	MemPct      float64
	HasRates    bool
	NetRxBps    float64
	NetTxBps    float64
	BlkReadBps  float64
	BlkWriteBps float64
}

// ComputeContainerMetrics calculates cgroup v1 & v2 compatible metrics from DockerStatsPayload
// and previous I/O counter state.
func ComputeContainerMetrics(payload DockerStatsPayload, prev *ioCounterState, nowTS int64) (ContainerMetrics, ioCounterState) {
	// 1. CPU Percentage Calculation
	var cpuPct float64
	curCPU := payload.CPUStats.CPUUsage.TotalUsage
	preCPU := payload.PreCPUStats.CPUUsage.TotalUsage
	curSys := payload.CPUStats.SystemCPUUsage
	preSys := payload.PreCPUStats.SystemCPUUsage

	// Guard against counter wraps / resets
	if curCPU >= preCPU && curSys > preSys {
		cpuDelta := float64(curCPU - preCPU)
		sysDelta := float64(curSys - preSys)

		onlineCPUs := payload.CPUStats.OnlineCPUs
		if onlineCPUs == 0 {
			onlineCPUs = uint32(len(payload.CPUStats.CPUUsage.PercpuUsage))
		}
		if onlineCPUs == 0 {
			onlineCPUs = 1
		}

		cpuPct = (cpuDelta / sysDelta) * float64(onlineCPUs) * 100.0
	}
	if cpuPct < 0 {
		cpuPct = 0
	}

	// 2. Memory Usage Calculation (subtract reclaimable page cache)
	// cgroup v2 uses "inactive_file", cgroup v1 uses "cache"
	var cache uint64
	if v, ok := payload.MemoryStats.Stats["inactive_file"]; ok && v > 0 {
		cache = v
	} else if v, ok := payload.MemoryStats.Stats["cache"]; ok && v > 0 {
		cache = v
	}

	usage := payload.MemoryStats.Usage
	if cache > usage {
		cache = usage
	}
	memUsed := float64(usage - cache)
	memLimit := float64(payload.MemoryStats.Limit)

	// 3. Network & Block I/O Counters
	var totalRx, totalTx uint64
	for _, netIf := range payload.Networks {
		totalRx += netIf.RxBytes
		totalTx += netIf.TxBytes
	}

	var totalBlkRead, totalBlkWrite uint64
	for _, blk := range payload.BlkioStats.IoServiceBytesRecursive {
		if strings.EqualFold(blk.Op, "read") {
			totalBlkRead += blk.Value
		} else if strings.EqualFold(blk.Op, "write") {
			totalBlkWrite += blk.Value
		}
	}

	newCounter := ioCounterState{
		TS:       nowTS,
		RxBytes:  totalRx,
		TxBytes:  totalTx,
		BlkRead:  totalBlkRead,
		BlkWrite: totalBlkWrite,
	}

	var memPct float64
	if memLimit > 0 {
		memPct = (memUsed / memLimit) * 100.0
		if memPct > 100.0 {
			memPct = 100.0
		}
	}

	res := ContainerMetrics{
		CPUPct:   cpuPct,
		MemUsed:  memUsed,
		MemLimit: memLimit,
		MemPct:   memPct,
	}

	// If previous baseline exists and time elapsed, calculate rates
	if prev != nil && prev.TS > 0 && nowTS > prev.TS {
		dt := float64(nowTS - prev.TS)
		res.HasRates = true

		if totalRx >= prev.RxBytes {
			res.NetRxBps = float64(totalRx-prev.RxBytes) / dt
		}
		if totalTx >= prev.TxBytes {
			res.NetTxBps = float64(totalTx-prev.TxBytes) / dt
		}
		if totalBlkRead >= prev.BlkRead {
			res.BlkReadBps = float64(totalBlkRead-prev.BlkRead) / dt
		}
		if totalBlkWrite >= prev.BlkWrite {
			res.BlkWriteBps = float64(totalBlkWrite-prev.BlkWrite) / dt
		}
	}

	return res, newCounter
}

// StatsCollector periodically polls container stats using a bounded worker pool
// and pushes calculated metrics into the telemetry pipeline.
type StatsCollector struct {
	client      *Client
	filter      *ContainerFilter
	sink        MetricSink
	interval    time.Duration
	concurrency int
	maxSources  int

	mu           sync.RWMutex
	containers   map[string]ContainerInfo  // id -> ContainerInfo
	prevCounters map[string]ioCounterState // id -> ioCounterState

	polling atomic.Bool
}

// StatsCollectorOptions configures a StatsCollector.
type StatsCollectorOptions struct {
	Interval    time.Duration
	Concurrency int
	MaxSources  int
}

// NewStatsCollector initializes a StatsCollector.
func NewStatsCollector(client *Client, filter *ContainerFilter, sink MetricSink, opts ...StatsCollectorOptions) *StatsCollector {
	interval := 10 * time.Second
	concurrency := 4
	maxSources := 500

	if len(opts) > 0 {
		if opts[0].Interval > 0 {
			interval = opts[0].Interval
		}
		if opts[0].Concurrency > 0 {
			concurrency = opts[0].Concurrency
		}
		if opts[0].MaxSources > 0 {
			maxSources = opts[0].MaxSources
		}
	}

	return &StatsCollector{
		client:       client,
		filter:       filter,
		sink:         sink,
		interval:     interval,
		concurrency:  concurrency,
		maxSources:   maxSources,
		containers:   make(map[string]ContainerInfo),
		prevCounters: make(map[string]ioCounterState),
	}
}

// HandleEvent updates the active container registry based on Docker lifecycle events.
func (c *StatsCollector) HandleEvent(ev ContainerEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()

	switch ev.Action {
	case "start", "restart":
		c.containers[ev.ContainerID] = ContainerInfo{
			ID:    ev.ContainerID,
			Name:  ev.ContainerName,
			Image: ev.Image,
			State: "running",
		}
	case "die", "stop", "destroy":
		delete(c.containers, ev.ContainerID)
		delete(c.prevCounters, ev.ContainerID)
	}
}

// SetContainers updates the internal map of active monitored containers.
func (c *StatsCollector) SetContainers(containers []ContainerInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()

	newMap := make(map[string]ContainerInfo, len(containers))
	for _, ci := range containers {
		if ci.State == "running" && (c.filter == nil || c.filter.Match(ci.Name)) {
			newMap[ci.ID] = ci
		}
	}
	c.containers = newMap

	// Prune dead containers from prevCounters
	for id := range c.prevCounters {
		if _, ok := newMap[id]; !ok {
			delete(c.prevCounters, id)
		}
	}
}

// RefreshContainers queries the Docker daemon for currently running containers.
func (c *StatsCollector) RefreshContainers(ctx context.Context) error {
	list, err := c.client.ListContainers(ctx, false)
	if err != nil {
		return err
	}
	c.SetContainers(list)
	return nil
}

// Run starts the periodic container telemetry collection loop.
func (c *StatsCollector) Run(ctx context.Context) {
	// Initial discovery
	_ = c.RefreshContainers(ctx)

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	// Initial immediate collection tick
	c.CollectOnce(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.CollectOnce(ctx)
		}
	}
}

// CollectOnce executes a single parallel polling sweep across all active monitored containers.
// Coalesces ticks to prevent overlapping collection cycles.
func (c *StatsCollector) CollectOnce(ctx context.Context) {
	if !c.polling.CompareAndSwap(false, true) {
		// Previous collection cycle still running; skip overlapping tick
		return
	}
	defer c.polling.Store(false)

	c.mu.RLock()
	targets := make([]ContainerInfo, 0, len(c.containers))
	for _, ci := range c.containers {
		targets = append(targets, ci)
		if len(targets) >= c.maxSources {
			break
		}
	}
	c.mu.RUnlock()

	if len(targets) == 0 {
		return
	}

	nowTS := time.Now().Unix()

	// Worker pool concurrency
	workerLimit := c.concurrency
	if workerLimit <= 0 {
		workerLimit = 4
	}
	if workerLimit > len(targets) {
		workerLimit = len(targets)
	}

	jobChan := make(chan ContainerInfo, len(targets))
	for _, t := range targets {
		jobChan <- t
	}
	close(jobChan)

	var wg sync.WaitGroup
	var sampleMu sync.Mutex
	collectedSamples := make([]model.MetricSample, 0, len(targets)*7)

	for w := 0; w < workerLimit; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			for ci := range jobChan {
				if ctx.Err() != nil {
					return
				}

				stats, err := c.client.GetContainerStats(ctx, ci.ID)
				if err != nil {
					continue
				}

				c.mu.Lock()
				var prevPtr *ioCounterState
				if p, ok := c.prevCounters[ci.ID]; ok {
					prevPtr = &p
				}
				metrics, nextCounter := ComputeContainerMetrics(stats, prevPtr, nowTS)
				c.prevCounters[ci.ID] = nextCounter
				c.mu.Unlock()

				entity := "container:" + ci.Name
				samples := []model.MetricSample{
					{Entity: entity, Metric: "cpu_pct", TS: nowTS, Value: metrics.CPUPct},
					{Entity: entity, Metric: "mem_used", TS: nowTS, Value: metrics.MemUsed},
					{Entity: entity, Metric: "mem_limit", TS: nowTS, Value: metrics.MemLimit},
					{Entity: entity, Metric: "mem_pct", TS: nowTS, Value: metrics.MemPct},
				}

				if metrics.HasRates {
					samples = append(samples,
						model.MetricSample{Entity: entity, Metric: "net_rx_bps", TS: nowTS, Value: metrics.NetRxBps},
						model.MetricSample{Entity: entity, Metric: "net_tx_bps", TS: nowTS, Value: metrics.NetTxBps},
						model.MetricSample{Entity: entity, Metric: "blk_read_bps", TS: nowTS, Value: metrics.BlkReadBps},
						model.MetricSample{Entity: entity, Metric: "blk_write_bps", TS: nowTS, Value: metrics.BlkWriteBps},
					)
				}

				sampleMu.Lock()
				collectedSamples = append(collectedSamples, samples...)
				sampleMu.Unlock()
			}
		}()
	}

	wg.Wait()

	if len(collectedSamples) > 0 && c.sink != nil {
		c.sink.Push(collectedSamples...)
	}
}
