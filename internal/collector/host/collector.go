package host

import (
	"context"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dale-tomson/Tarsier/internal/config"
	"github.com/dale-tomson/Tarsier/internal/model"
)

// CollectorOption defines functional configuration for HostCollector.
type CollectorOption func(*HostCollector)

// WithProcDir overrides the proc directory path.
func WithProcDir(dir string) CollectorOption {
	return func(c *HostCollector) {
		if dir != "" {
			c.hostProc = dir
		}
	}
}

// WithSysDir overrides the sys directory path.
func WithSysDir(dir string) CollectorOption {
	return func(c *HostCollector) {
		if dir != "" {
			c.hostSys = dir
		}
	}
}

// WithRootDir overrides the host root mount directory path.
func WithRootDir(dir string) CollectorOption {
	return func(c *HostCollector) {
		if dir != "" {
			c.hostRoot = dir
		}
	}
}

// WithInterval overrides the collection interval.
func WithInterval(d time.Duration) CollectorOption {
	return func(c *HostCollector) {
		if d > 0 {
			c.interval = d
		}
	}
}

// HostCollector periodically gathers host telemetry and emits samples onto an outbound channel.
type HostCollector struct {
	cfg      config.HostConfig
	out      chan<- model.MetricSample
	hostProc string
	hostSys  string
	hostRoot string
	interval time.Duration

	mu          sync.Mutex
	hasBaseline bool
	prevCPUs    map[string]cpuTicks
	prevDiskIO  diskIOSnapshot
	prevNetIO   netIOSnapshot
	prevTime    time.Time

	dropped atomic.Uint64
}

// NewCollector constructs a HostCollector using HostConfig and environment defaults.
func NewCollector(cfg config.HostConfig, out chan<- model.MetricSample, opts ...CollectorOption) (*HostCollector, error) {
	procDir := os.Getenv("HOST_PROC")
	if procDir == "" {
		procDir = "/proc"
	}

	sysDir := os.Getenv("HOST_SYS")
	if sysDir == "" {
		sysDir = "/sys"
	}

	rootDir := os.Getenv("HOST_ROOT")
	if rootDir == "" {
		rootDir = "/"
	}

	interval := 10 * time.Second
	if cfg.Interval != "" {
		if d, err := time.ParseDuration(cfg.Interval); err == nil && d > 0 {
			interval = d
		}
	}

	c := &HostCollector{
		cfg:      cfg,
		out:      out,
		hostProc: procDir,
		hostSys:  sysDir,
		hostRoot: rootDir,
		interval: interval,
		prevCPUs: make(map[string]cpuTicks),
	}

	for _, opt := range opts {
		opt(c)
	}

	return c, nil
}

// Dropped returns the total number of dropped samples due to saturated outbound channel.
func (c *HostCollector) Dropped() uint64 {
	return c.dropped.Load()
}

// emit sends a sample non-blockingly, incrementing dropped counter on saturation.
func (c *HostCollector) emit(ctx context.Context, s model.MetricSample) {
	select {
	case c.out <- s:
	case <-ctx.Done():
	default:
		c.dropped.Add(1)
	}
}

// Collect executes a single telemetry gathering pass across host subsystems.
func (c *HostCollector) Collect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	nowUnix := now.Unix()

	// 1. CPU Ticks
	currCPUs, errCPU := readCPUTicks(c.hostProc)
	if errCPU != nil {
		log.Printf("host collector: warning reading cpu stat: %v", errCPU)
	}

	// 2. Load Average (Gauge)
	if loadSamples, err := readLoadAvg(c.hostProc, nowUnix); err == nil {
		for _, s := range loadSamples {
			c.emit(ctx, s)
		}
	} else {
		log.Printf("host collector: warning reading loadavg: %v", err)
	}

	// 3. Memory & Swap (Gauge)
	if memSamples, err := readMemoryMetrics(c.hostProc, nowUnix); err == nil {
		for _, s := range memSamples {
			c.emit(ctx, s)
		}
	} else {
		log.Printf("host collector: warning reading meminfo: %v", err)
	}

	// 4. Filesystem Discovery & Space (Gauge)
	mounts, errMounts := discoverMounts(c.hostProc, c.cfg.Disks, c.cfg.DiskExcludeFS)
	if errMounts == nil {
		spaceSamples := readDiskSpace(mounts, c.hostRoot, nowUnix)
		for _, s := range spaceSamples {
			c.emit(ctx, s)
		}
	} else {
		log.Printf("host collector: warning discovering mounts: %v", errMounts)
	}

	// 5. Disk I/O Counters
	currDiskIO, errDiskIO := readDiskIO(c.hostProc)
	if errDiskIO != nil {
		log.Printf("host collector: warning reading diskstats: %v", errDiskIO)
	}

	// 6. Network I/O Counters
	currNetIO, errNetIO := readNetIO(c.hostProc)
	if errNetIO != nil {
		log.Printf("host collector: warning reading net/dev: %v", errNetIO)
	}

	// Check if this is the first tick (baseline capture)
	if !c.hasBaseline {
		if errCPU == nil {
			c.prevCPUs = currCPUs
		}
		if errDiskIO == nil {
			c.prevDiskIO = currDiskIO
		}
		if errNetIO == nil {
			c.prevNetIO = currNetIO
		}
		c.prevTime = now
		c.hasBaseline = true
		return nil
	}

	// Calculate delta elapsed time
	deltaSec := now.Sub(c.prevTime).Seconds()
	if deltaSec <= 0 {
		deltaSec = 1.0
	}

	// Emit CPU % deltas
	if errCPU == nil {
		if prevTotal, ok := c.prevCPUs["cpu"]; ok {
			if currTotal, ok := currCPUs["cpu"]; ok {
				cpuPct := computeCPUPercentage(prevTotal, currTotal)
				c.emit(ctx, model.MetricSample{
					Entity: "host",
					Metric: "cpu_pct",
					TS:     nowUnix,
					Value:  cpuPct,
				})
			}
		}
		c.prevCPUs = currCPUs
	}

	// Emit Disk I/O bps deltas
	if errDiskIO == nil {
		if currDiskIO.readBytes >= c.prevDiskIO.readBytes {
			readBps := float64(currDiskIO.readBytes-c.prevDiskIO.readBytes) / deltaSec
			c.emit(ctx, model.MetricSample{
				Entity: "host",
				Metric: "disk_read_bps",
				TS:     nowUnix,
				Value:  readBps,
			})
		}
		if currDiskIO.writeBytes >= c.prevDiskIO.writeBytes {
			writeBps := float64(currDiskIO.writeBytes-c.prevDiskIO.writeBytes) / deltaSec
			c.emit(ctx, model.MetricSample{
				Entity: "host",
				Metric: "disk_write_bps",
				TS:     nowUnix,
				Value:  writeBps,
			})
		}
		c.prevDiskIO = currDiskIO
	}

	// Emit Network I/O bps deltas
	if errNetIO == nil {
		if currNetIO.rxBytes >= c.prevNetIO.rxBytes {
			rxBps := float64(currNetIO.rxBytes-c.prevNetIO.rxBytes) / deltaSec
			c.emit(ctx, model.MetricSample{
				Entity: "host",
				Metric: "net_rx_bps",
				TS:     nowUnix,
				Value:  rxBps,
			})
		}
		if currNetIO.txBytes >= c.prevNetIO.txBytes {
			txBps := float64(currNetIO.txBytes-c.prevNetIO.txBytes) / deltaSec
			c.emit(ctx, model.MetricSample{
				Entity: "host",
				Metric: "net_tx_bps",
				TS:     nowUnix,
				Value:  txBps,
			})
		}
		c.prevNetIO = currNetIO
	}

	c.prevTime = now
	return nil
}

// Run executes the periodic collection ticker loop until ctx cancels.
func (c *HostCollector) Run(ctx context.Context) {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	// Initial collection pass
	_ = c.Collect(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = c.Collect(ctx)
		}
	}
}
