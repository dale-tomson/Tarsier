package host

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dale-tomson/Tarsier/internal/config"
	"github.com/dale-tomson/Tarsier/internal/model"
)

func TestParseCPU_DeltaCalculation(t *testing.T) {
	// Snapshot 1: total = 1000 + 200 + 300 + 3500 = 5000; idle = 3500
	snap1 := cpuTicks{total: 5000, idleAll: 3500}

	// Snapshot 2: user +250, system +50, idle +700 => total +1000, idle +700
	// Utilization = (1000 - 700) / 1000 * 100 = 30.0%
	snap2 := cpuTicks{total: 6000, idleAll: 4200}

	pct := computeCPUPercentage(snap1, snap2)
	if math.Abs(pct-30.0) > 0.01 {
		t.Fatalf("expected 30.0%% CPU, got %f", pct)
	}

	// Zero-delta snapshot must return 0.0 without NaN
	zeroPct := computeCPUPercentage(snap2, snap2)
	if zeroPct != 0.0 || math.IsNaN(zeroPct) {
		t.Fatalf("expected 0.0 CPU on zero delta, got %f", zeroPct)
	}
}

func TestParseMemory(t *testing.T) {
	tmpDir := t.TempDir()
	meminfoContent := `MemTotal:       16384000 kB
MemFree:         4096000 kB
MemAvailable:    8192000 kB
Buffers:          512000 kB
Cached:          2048000 kB
SwapCached:            0 kB
Active:          3000000 kB
Inactive:        1500000 kB
SReclaimable:     400000 kB
SwapTotal:       2097152 kB
SwapFree:        1048576 kB
`
	if err := os.WriteFile(filepath.Join(tmpDir, "meminfo"), []byte(meminfoContent), 0644); err != nil {
		t.Fatalf("write meminfo: %v", err)
	}

	now := int64(1731650000)
	samples, err := readMemoryMetrics(tmpDir, now)
	if err != nil {
		t.Fatalf("readMemoryMetrics failed: %v", err)
	}

	m := make(map[string]float64)
	for _, s := range samples {
		m[s.Metric] = s.Value
	}

	// Expected values:
	// MemTotal = 16384000 * 1024 = 16777216000
	// MemAvailable = 8192000 * 1024 = 8388608000
	// MemUsed = 16777216000 - 8388608000 = 8388608000
	// MemUsedPct = 50.0%
	// MemCached = (2048000 + 512000 + 400000) * 1024 = 3031040000
	// SwapTotal = 2097152 * 1024 = 2147483648
	// SwapUsed = (2097152 - 1048576) * 1024 = 1073741824

	if m["mem_total"] != 16777216000 {
		t.Errorf("mem_total: expected 16777216000, got %f", m["mem_total"])
	}
	if m["mem_available"] != 8388608000 {
		t.Errorf("mem_available: expected 8388608000, got %f", m["mem_available"])
	}
	if m["mem_used"] != 8388608000 {
		t.Errorf("mem_used: expected 8388608000, got %f", m["mem_used"])
	}
	if math.Abs(m["mem_used_pct"]-50.0) > 0.01 {
		t.Errorf("mem_used_pct: expected 50.0, got %f", m["mem_used_pct"])
	}
	if m["mem_cached"] != 3031040000 {
		t.Errorf("mem_cached: expected 3031040000, got %f", m["mem_cached"])
	}
	if m["swap_total"] != 2147483648 {
		t.Errorf("swap_total: expected 2147483648, got %f", m["swap_total"])
	}
	if m["swap_used"] != 1073741824 {
		t.Errorf("swap_used: expected 1073741824, got %f", m["swap_used"])
	}
}

func TestDiscoverMounts_FilteringAndDeduplication(t *testing.T) {
	tmpDir := t.TempDir()
	proc1Dir := filepath.Join(tmpDir, "1")
	if err := os.MkdirAll(proc1Dir, 0755); err != nil {
		t.Fatalf("mkdir proc/1: %v", err)
	}

	mountinfoContent := `36 35 98:0 / / rw,noatime master:1 - ext4 /dev/root rw,errors=remount-ro
37 36 98:0 / /var/lib/docker/bind rw master:1 - ext4 /dev/root rw
38 36 0:25 / /sys rw,nosuid,nodev,noexec,relatime - sysfs sysfs rw
39 36 0:26 / /proc rw,nosuid,nodev,noexec,relatime - proc proc rw
40 36 0:27 / /dev rw,nosuid - devtmpfs udev rw
41 36 0:28 / /dev/shm rw,nosuid,nodev - tmpfs tmpfs rw
42 36 259:1 / /mnt/data rw,relatime - xfs /dev/nvme0n1p1 rw
43 36 0:50 / /var/lib/docker/overlay2/abc/merged rw - overlay overlay rw
`
	if err := os.WriteFile(filepath.Join(proc1Dir, "mountinfo"), []byte(mountinfoContent), 0644); err != nil {
		t.Fatalf("write mountinfo: %v", err)
	}

	mounts, err := discoverMounts(tmpDir, nil, nil)
	if err != nil {
		t.Fatalf("discoverMounts failed: %v", err)
	}

	mountMap := make(map[string]bool)
	for _, m := range mounts {
		mountMap[m] = true
	}

	// Pseudo filesystems (proc, sys, devtmpfs, tmpfs, overlay) must be excluded
	if mountMap["/proc"] || mountMap["/sys"] || mountMap["/dev"] || mountMap["/dev/shm"] || mountMap["/var/lib/docker/overlay2/abc/merged"] {
		t.Fatalf("found pseudo-fs in discovered mounts: %v", mounts)
	}

	// ext4 root device (98:0) had two mounts: "/" and "/var/lib/docker/bind" -> must deduplicate to "/"
	if !mountMap["/"] {
		t.Errorf("expected '/' mount point")
	}
	if mountMap["/var/lib/docker/bind"] {
		t.Errorf("duplicate bind mount '/var/lib/docker/bind' was not deduplicated")
	}

	// xfs (/mnt/data) should be present
	if !mountMap["/mnt/data"] {
		t.Errorf("expected '/mnt/data' mount point")
	}
}

func TestParseNetwork_ExcludesVirtualInterfaces(t *testing.T) {
	tmpDir := t.TempDir()
	netDir := filepath.Join(tmpDir, "net")
	if err := os.MkdirAll(netDir, 0755); err != nil {
		t.Fatalf("mkdir net: %v", err)
	}

	netdev1 := `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 1000000    1000    0    0    0     0          0         0  1000000    1000    0    0    0     0       0          0
  eth0: 5000000    5000    0    0    0     0          0         0  2000000    2000    0    0    0     0       0          0
veth12:  500000     500    0    0    0     0          0         0   500000     500    0    0    0     0       0          0
docker0: 800000     800    0    0    0     0          0         0   800000     800    0    0    0     0       0          0
`
	if err := os.WriteFile(filepath.Join(netDir, "dev"), []byte(netdev1), 0644); err != nil {
		t.Fatalf("write net/dev: %v", err)
	}

	snap1, err := readNetIO(tmpDir)
	if err != nil {
		t.Fatalf("readNetIO failed: %v", err)
	}

	// Only eth0 should be counted: rx = 5,000,000, tx = 2,000,000
	if snap1.rxBytes != 5000000 || snap1.txBytes != 2000000 {
		t.Fatalf("expected eth0-only counters (rx: 5000000, tx: 2000000), got rx: %d, tx: %d", snap1.rxBytes, snap1.txBytes)
	}

	netdev2 := `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 2000000    2000    0    0    0     0          0         0  2000000    2000    0    0    0     0       0          0
  eth0: 8000000    8000    0    0    0     0          0         0  4000000    4000    0    0    0     0       0          0
veth12: 1500000    1500    0    0    0     0          0         0  1500000    1500    0    0    0     0       0          0
docker0:1800000    1800    0    0    0     0          0         0  1800000    1800    0    0    0     0       0          0
`
	if err := os.WriteFile(filepath.Join(netDir, "dev"), []byte(netdev2), 0644); err != nil {
		t.Fatalf("write net/dev 2: %v", err)
	}

	snap2, err := readNetIO(tmpDir)
	if err != nil {
		t.Fatalf("readNetIO 2 failed: %v", err)
	}

	// delta rx: 3,000,000 over 1 sec = 3,000,000 bps
	// delta tx: 2,000,000 over 1 sec = 2,000,000 bps
	deltaRx := snap2.rxBytes - snap1.rxBytes
	deltaTx := snap2.txBytes - snap1.txBytes
	if deltaRx != 3000000 || deltaTx != 2000000 {
		t.Fatalf("expected deltaRx=3000000, deltaTx=2000000, got %d, %d", deltaRx, deltaTx)
	}
}

func TestCollector_LifecycleAndNonBlocking(t *testing.T) {
	tmpDir := t.TempDir()

	// Write initial mock proc fixtures
	stat1 := "cpu  1000 200 300 3500 100 0 0 0 0 0\n"
	_ = os.WriteFile(filepath.Join(tmpDir, "stat"), []byte(stat1), 0644)
	_ = os.WriteFile(filepath.Join(tmpDir, "loadavg"), []byte("0.50 0.30 0.20 1/100 1234\n"), 0644)
	meminfo := "MemTotal: 1000000 kB\nMemFree: 500000 kB\nMemAvailable: 500000 kB\nBuffers: 0 kB\nCached: 0 kB\nSReclaimable: 0 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n"
	_ = os.WriteFile(filepath.Join(tmpDir, "meminfo"), []byte(meminfo), 0644)
	diskstats1 := "   8       0 sda 10 0 100 50 20 0 200 60 0 110 110\n"
	_ = os.WriteFile(filepath.Join(tmpDir, "diskstats"), []byte(diskstats1), 0644)
	netDir := filepath.Join(tmpDir, "net")
	_ = os.MkdirAll(netDir, 0755)
	netdev1 := "Inter-|   Receive\n face |bytes\n  eth0: 1000 0 0 0 0 0 0 0 2000 0 0 0 0 0 0 0\n"
	_ = os.WriteFile(filepath.Join(netDir, "dev"), []byte(netdev1), 0644)

	// Mock statfsFunc
	origStatfs := statfsFunc
	defer func() { statfsFunc = origStatfs }()
	statfsFunc = func(path string) (StatfsData, error) {
		return StatfsData{
			Blocks: 1000000,
			Bfree:  400000,
			Bavail: 400000,
			Bsize:  4096,
		}, nil
	}

	out := make(chan model.MetricSample, 100)
	c, err := NewCollector(
		config.HostConfig{Disks: []string{"/"}},
		out,
		WithProcDir(tmpDir),
		WithInterval(50*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("NewCollector failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Tick 1: Baseline suppression test
	if err := c.Collect(ctx); err != nil {
		t.Fatalf("Collect tick 1 failed: %v", err)
	}

	// Drain tick 1 samples: only gauge metrics should exist, NO delta metrics
	var tick1Samples []model.MetricSample
drain1:
	for {
		select {
		case s := <-out:
			tick1Samples = append(tick1Samples, s)
		default:
			break drain1
		}
	}

	for _, s := range tick1Samples {
		if s.Metric == "cpu_pct" || s.Metric == "disk_read_bps" || s.Metric == "disk_write_bps" || s.Metric == "net_rx_bps" || s.Metric == "net_tx_bps" {
			t.Fatalf("delta metric %q was emitted on tick 1 baseline", s.Metric)
		}
	}
	if len(tick1Samples) == 0 {
		t.Fatalf("expected gauge metrics on tick 1, got 0 samples")
	}

	// Update proc files for tick 2
	time.Sleep(20 * time.Millisecond)
	stat2 := "cpu  1250 200 350 4200 100 0 0 0 0 0\n"
	_ = os.WriteFile(filepath.Join(tmpDir, "stat"), []byte(stat2), 0644)
	diskstats2 := "   8       0 sda 20 0 200 50 30 0 300 60 0 110 110\n"
	_ = os.WriteFile(filepath.Join(tmpDir, "diskstats"), []byte(diskstats2), 0644)
	netdev2 := "Inter-|   Receive\n face |bytes\n  eth0: 3000 0 0 0 0 0 0 0 4000 0 0 0 0 0 0 0\n"
	_ = os.WriteFile(filepath.Join(netDir, "dev"), []byte(netdev2), 0644)

	// Tick 2: Deltas should now be emitted
	if err := c.Collect(ctx); err != nil {
		t.Fatalf("Collect tick 2 failed: %v", err)
	}

	var tick2Samples []model.MetricSample
drain2:
	for {
		select {
		case s := <-out:
			tick2Samples = append(tick2Samples, s)
		default:
			break drain2
		}
	}

	metricsFound := make(map[string]bool)
	for _, s := range tick2Samples {
		metricsFound[s.Metric] = true
	}

	expectedDeltas := []string{"cpu_pct", "disk_read_bps", "disk_write_bps", "net_rx_bps", "net_tx_bps"}
	for _, m := range expectedDeltas {
		if !metricsFound[m] {
			t.Errorf("expected delta metric %q was not emitted on tick 2", m)
		}
	}

	// Run background collector and test clean cancellation
	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()

	time.Sleep(70 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// Clean exit
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("Run did not exit cleanly on context cancellation")
	}
}
