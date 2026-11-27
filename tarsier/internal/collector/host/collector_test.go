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

func TestComputeCPUPercentage_Table(t *testing.T) {
	tests := []struct {
		name string
		prev cpuTicks
		curr cpuTicks
		want float64
	}{
		{
			name: "normal tick delta 30 percent",
			prev: cpuTicks{total: 5000, idleAll: 3500},
			curr: cpuTicks{total: 6000, idleAll: 4200},
			want: 30.0,
		},
		{
			name: "zero tick delta returns zero without NaN",
			prev: cpuTicks{total: 6000, idleAll: 4200},
			curr: cpuTicks{total: 6000, idleAll: 4200},
			want: 0.0,
		},
		{
			name: "100 percent utilization",
			prev: cpuTicks{total: 5000, idleAll: 3500},
			curr: cpuTicks{total: 6000, idleAll: 3500},
			want: 100.0,
		},
		{
			name: "idle equals total delta",
			prev: cpuTicks{total: 5000, idleAll: 3500},
			curr: cpuTicks{total: 6000, idleAll: 4500},
			want: 0.0,
		},
		{
			name: "negative or zero total ticks delta",
			prev: cpuTicks{total: 6000, idleAll: 4200},
			curr: cpuTicks{total: 5000, idleAll: 4000},
			want: 0.0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pct := computeCPUPercentage(tc.prev, tc.curr)
			if math.IsNaN(pct) {
				t.Fatalf("computeCPUPercentage returned NaN")
			}
			if math.Abs(pct-tc.want) > 0.01 {
				t.Fatalf("expected %.2f%% CPU, got %f", tc.want, pct)
			}
		})
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
	tests := []struct {
		name            string
		mountinfo       string
		configuredDisks []string
		excludeFS       []string
		wantPresent     []string
		wantAbsent      []string
	}{
		{
			name: "standard host with pseudo-fs and bind mount deduplication",
			mountinfo: `36 35 98:0 / / rw,noatime master:1 - ext4 /dev/root rw,errors=remount-ro
37 36 98:0 / /var/lib/docker/bind rw master:1 - ext4 /dev/root rw
38 36 0:25 / /sys rw,nosuid,nodev,noexec,relatime - sysfs sysfs rw
39 36 0:26 / /proc rw,nosuid,nodev,noexec,relatime - proc proc rw
40 36 0:27 / /dev rw,nosuid - devtmpfs udev rw
41 36 0:28 / /dev/shm rw,nosuid,nodev - tmpfs tmpfs rw
42 36 259:1 / /mnt/data rw,relatime - xfs /dev/nvme0n1p1 rw
43 36 0:50 / /var/lib/docker/overlay2/abc/merged rw - overlay overlay rw
`,
			wantPresent: []string{"/", "/mnt/data"},
			wantAbsent:  []string{"/proc", "/sys", "/dev", "/dev/shm", "/var/lib/docker/overlay2/abc/merged", "/var/lib/docker/bind"},
		},
		{
			name: "container overlay root preservation",
			mountinfo: `50 49 0:60 / / rw,relatime - overlay overlay rw,lowerdir=/var/lib/docker
51 50 98:0 / /data rw - ext4 /dev/sda1 rw
`,
			wantPresent: []string{"/", "/data"},
		},
		{
			name: "include and exclude glob filtering",
			mountinfo: `36 35 98:0 / / rw,noatime master:1 - ext4 /dev/root rw
42 36 259:1 / /mnt/data rw,relatime - xfs /dev/nvme0n1p1 rw
44 36 0:61 / /mnt/nfs rw,relatime - nfs4 10.0.0.1:/share rw
45 36 8:16 / /data rw,relatime - ext4 /dev/sdb1 rw
`,
			configuredDisks: []string{"/", "/data"},
			excludeFS:       []string{"xfs", " nfs4 "},
			wantPresent:     []string{"/", "/data"},
			wantAbsent:      []string{"/mnt/data", "/mnt/nfs"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			proc1Dir := filepath.Join(tmpDir, "1")
			if err := os.MkdirAll(proc1Dir, 0755); err != nil {
				t.Fatalf("mkdir proc/1: %v", err)
			}
			if err := os.WriteFile(filepath.Join(proc1Dir, "mountinfo"), []byte(tc.mountinfo), 0644); err != nil {
				t.Fatalf("write mountinfo: %v", err)
			}

			mounts, err := discoverMounts(tmpDir, nil, tc.excludeFS)
			if err != nil {
				t.Fatalf("discoverMounts failed: %v", err)
			}

			mountMap := make(map[string]bool, len(mounts))
			for _, m := range mounts {
				mountMap[m] = true
			}

			for _, want := range tc.wantPresent {
				if !mountMap[want] {
					t.Errorf("expected mount point %q in %v", want, mounts)
				}
			}
			for _, absent := range tc.wantAbsent {
				if mountMap[absent] {
					t.Errorf("unexpected mount point %q in %v", absent, mounts)
				}
			}

			if len(tc.configuredDisks) > 0 {
				included, err := discoverMounts(tmpDir, tc.configuredDisks, tc.excludeFS)
				if err != nil {
					t.Fatalf("discoverMounts with configuredDisks failed: %v", err)
				}
				if len(included) != len(tc.configuredDisks) {
					t.Fatalf("expected %d configured mounts, got %v", len(tc.configuredDisks), included)
				}
				for i, want := range tc.configuredDisks {
					if included[i] != want {
						t.Errorf("configured mount[%d]: expected %q, got %q", i, want, included[i])
					}
				}
			}
		})
	}
}

func TestParseNetwork_ExcludesVirtualInterfaces(t *testing.T) {
	tests := []struct {
		name        string
		netdev1     string
		netdev2     string
		hasSecond   bool
		wantRx1     uint64
		wantTx1     uint64
		wantDeltaRx uint64
		wantDeltaTx uint64
	}{
		{
			name: "excludes lo, veth, and bridge interfaces",
			netdev1: `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 1000000    1000    0    0    0     0          0         0  1000000    1000    0    0    0     0       0          0
  eth0: 5000000    5000    0    0    0     0          0         0  2000000    2000    0    0    0     0       0          0
veth12:  500000     500    0    0    0     0          0         0   500000     500    0    0    0     0       0          0
docker0: 800000     800    0    0    0     0          0         0   800000     800    0    0    0     0       0          0
br-123:  400000     400    0    0    0     0          0         0   400000     400    0    0    0     0       0          0
`,
			netdev2: `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 2000000    2000    0    0    0     0          0         0  2000000    2000    0    0    0     0       0          0
  eth0: 8000000    8000    0    0    0     0          0         0  4000000    4000    0    0    0     0       0          0
veth12: 1500000    1500    0    0    0     0          0         0  1500000    1500    0    0    0     0       0          0
docker0:1800000    1800    0    0    0     0          0         0  1800000    1800    0    0    0     0       0          0
br-123: 1400000    1400    0    0    0     0          0         0  1400000    1400    0    0    0     0       0          0
`,
			hasSecond:   true,
			wantRx1:     5000000,
			wantTx1:     2000000,
			wantDeltaRx: 3000000,
			wantDeltaTx: 2000000,
		},
		{
			name: "handles empty or malformed net dev file",
			netdev1: `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
malformed_no_colon
short: 100 1 0
badnum: notanumber 0 0 0 0 0 0 0 badtx 0 0 0 0 0 0 0
`,
			netdev2:     "",
			hasSecond:   true,
			wantRx1:     0,
			wantTx1:     0,
			wantDeltaRx: 0,
			wantDeltaTx: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			netDir := filepath.Join(tmpDir, "net")
			if err := os.MkdirAll(netDir, 0755); err != nil {
				t.Fatalf("mkdir net: %v", err)
			}

			if err := os.WriteFile(filepath.Join(netDir, "dev"), []byte(tc.netdev1), 0644); err != nil {
				t.Fatalf("write net/dev: %v", err)
			}

			snap1, err := readNetIO(tmpDir)
			if err != nil {
				t.Fatalf("readNetIO failed: %v", err)
			}
			if snap1.rxBytes != tc.wantRx1 || snap1.txBytes != tc.wantTx1 {
				t.Fatalf("expected counters (rx: %d, tx: %d), got rx: %d, tx: %d", tc.wantRx1, tc.wantTx1, snap1.rxBytes, snap1.txBytes)
			}

			if tc.hasSecond {
				if err := os.WriteFile(filepath.Join(netDir, "dev"), []byte(tc.netdev2), 0644); err != nil {
					t.Fatalf("write net/dev 2: %v", err)
				}

				snap2, err := readNetIO(tmpDir)
				if err != nil {
					t.Fatalf("readNetIO 2 failed: %v", err)
				}

				deltaRx := snap2.rxBytes - snap1.rxBytes
				deltaTx := snap2.txBytes - snap1.txBytes
				if deltaRx != tc.wantDeltaRx || deltaTx != tc.wantDeltaTx {
					t.Fatalf("expected deltaRx=%d, deltaTx=%d, got %d, %d", tc.wantDeltaRx, tc.wantDeltaTx, deltaRx, deltaTx)
				}
			}
		})
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
