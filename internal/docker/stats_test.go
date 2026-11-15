package docker

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dale-tomson/Tarsier/internal/db"
	"github.com/dale-tomson/Tarsier/internal/metrics"
	"github.com/dale-tomson/Tarsier/internal/model"
)

type mockSink struct {
	mu      sync.Mutex
	samples []model.MetricSample
}

func (s *mockSink) Push(samples ...model.MetricSample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.samples = append(s.samples, samples...)
}

func (s *mockSink) GetSamples() []model.MetricSample {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]model.MetricSample, len(s.samples))
	copy(out, s.samples)
	return out
}

func (s *mockSink) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.samples = nil
}

func TestComputeStats_CgroupV1AndV2(t *testing.T) {
	// 1. Multicore cgroup v2 payload
	var payloadV2 DockerStatsPayload
	payloadV2.CPUStats.CPUUsage.TotalUsage = 2000000000   // 2s CPU
	payloadV2.PreCPUStats.CPUUsage.TotalUsage = 1000000000 // 1s CPU -> delta 1s
	payloadV2.CPUStats.SystemCPUUsage = 20000000000        // 20s Sys
	payloadV2.PreCPUStats.SystemCPUUsage = 10000000000     // 10s Sys -> delta 10s
	payloadV2.CPUStats.OnlineCPUs = 4
	payloadV2.MemoryStats.Usage = 104857600  // 100 MiB
	payloadV2.MemoryStats.Limit = 1073741824 // 1 GiB
	payloadV2.MemoryStats.Stats = map[string]uint64{
		"inactive_file": 20971520, // 20 MiB cgroup v2 cache
	}

	m2, _ := ComputeContainerMetrics(payloadV2, nil, 1700000010)

	// CPU % = (1s / 10s) * 4 * 100 = 40.0%
	if math.Abs(m2.CPUPct-40.0) > 0.001 {
		t.Errorf("expected CPU 40.0%%, got %.2f%%", m2.CPUPct)
	}
	// Mem Used = 100 MiB - 20 MiB = 80 MiB (83886080 bytes)
	if m2.MemUsed != 83886080 {
		t.Errorf("expected MemUsed 83886080, got %.0f", m2.MemUsed)
	}
	if m2.MemLimit != 1073741824 {
		t.Errorf("expected MemLimit 1073741824, got %.0f", m2.MemLimit)
	}

	// 2. cgroup v1 memory subtraction (using "cache")
	var payloadV1 DockerStatsPayload
	payloadV1.MemoryStats.Usage = 52428800 // 50 MiB
	payloadV1.MemoryStats.Stats = map[string]uint64{
		"cache": 10485760, // 10 MiB cgroup v1 cache
	}
	m1, _ := ComputeContainerMetrics(payloadV1, nil, 1700000010)
	if m1.MemUsed != 41943040 { // 40 MiB
		t.Errorf("expected cgroup v1 MemUsed 41943040, got %.0f", m1.MemUsed)
	}

	// 3. Counter reset / underflow protection
	var payloadReset DockerStatsPayload
	payloadReset.CPUStats.CPUUsage.TotalUsage = 500000
	payloadReset.PreCPUStats.CPUUsage.TotalUsage = 1000000 // Underflow condition
	payloadReset.CPUStats.SystemCPUUsage = 2000000
	payloadReset.PreCPUStats.SystemCPUUsage = 1000000

	mReset, _ := ComputeContainerMetrics(payloadReset, nil, 1700000010)
	if mReset.CPUPct != 0 {
		t.Errorf("expected CPU 0 on counter reset, got %.2f", mReset.CPUPct)
	}
}

func TestStatsCollector_NetworkAndBlockIORates(t *testing.T) {
	// First tick (establish baseline)
	var tick1 DockerStatsPayload
	tick1.Networks = map[string]struct {
		RxBytes uint64 `json:"rx_bytes"`
		TxBytes uint64 `json:"tx_bytes"`
	}{
		"eth0": {RxBytes: 10000, TxBytes: 20000},
	}
	tick1.BlkioStats.IoServiceBytesRecursive = []struct {
		Op    string `json:"op"`
		Value uint64 `json:"value"`
	}{
		{Op: "Read", Value: 50000},
		{Op: "Write", Value: 60000},
	}

	m1, counter1 := ComputeContainerMetrics(tick1, nil, 1700000000)
	if m1.HasRates {
		t.Errorf("first sample must not emit rates without baseline")
	}

	// Second tick (10 seconds later: +10,000 rx, +20,000 tx, +5,000 read, +15,000 write)
	var tick2 DockerStatsPayload
	tick2.Networks = map[string]struct {
		RxBytes uint64 `json:"rx_bytes"`
		TxBytes uint64 `json:"tx_bytes"`
	}{
		"eth0": {RxBytes: 20000, TxBytes: 40000},
	}
	tick2.BlkioStats.IoServiceBytesRecursive = []struct {
		Op    string `json:"op"`
		Value uint64 `json:"value"`
	}{
		{Op: "Read", Value: 55000},
		{Op: "Write", Value: 75000},
	}

	m2, _ := ComputeContainerMetrics(tick2, &counter1, 1700000010)
	if !m2.HasRates {
		t.Fatalf("expected HasRates to be true for second sample")
	}

	// Rate = delta / 10s
	// rx: 10,000 / 10 = 1,000 B/s
	// tx: 20,000 / 10 = 2,000 B/s
	// blk_read: 5,000 / 10 = 500 B/s
	// blk_write: 15,000 / 10 = 1,500 B/s
	if m2.NetRxBps != 1000 {
		t.Errorf("expected NetRxBps 1000, got %.2f", m2.NetRxBps)
	}
	if m2.NetTxBps != 2000 {
		t.Errorf("expected NetTxBps 2000, got %.2f", m2.NetTxBps)
	}
	if m2.BlkReadBps != 500 {
		t.Errorf("expected BlkReadBps 500, got %.2f", m2.BlkReadBps)
	}
	if m2.BlkWriteBps != 1500 {
		t.Errorf("expected BlkWriteBps 1500, got %.2f", m2.BlkWriteBps)
	}
}

func TestStatsCollector_BoundedPoolAndRollupIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var inFlight atomic.Int32
	var maxConcurrent atomic.Int32

	mux := http.NewServeMux()
	mux.HandleFunc("/containers/", func(w http.ResponseWriter, r *http.Request) {
		current := inFlight.Add(1)
		for {
			prev := maxConcurrent.Load()
			if current <= prev || maxConcurrent.CompareAndSwap(prev, current) {
				break
			}
		}
		defer inFlight.Add(-1)

		// Artificial 15ms latency to test concurrency pooling
		time.Sleep(15 * time.Millisecond)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"read": "2026-11-15T12:00:10Z",
			"precpu_stats": {"cpu_usage": {"total_usage": 1000000000}, "system_cpu_usage": 10000000000},
			"cpu_stats": {"cpu_usage": {"total_usage": 1200000000}, "system_cpu_usage": 12000000000, "online_cpus": 2},
			"memory_stats": {"usage": 52428800, "limit": 268435456, "stats": {"inactive_file": 10485760}}
		}`))
	})

	socketPath := newMockDockerServer(t, mux)
	client := NewClient(socketPath)

	// Create 12 container targets with concurrency capped at 3
	containers := make([]ContainerInfo, 12)
	for i := 0; i < 12; i++ {
		containers[i] = ContainerInfo{
			ID:    fmt.Sprintf("cid-%02d", i),
			Name:  fmt.Sprintf("service-%02d", i),
			State: "running",
		}
	}

	sink := &mockSink{}
	collector := NewStatsCollector(client, nil, sink, StatsCollectorOptions{
		Interval:    100 * time.Millisecond,
		Concurrency: 3,
	})
	collector.SetContainers(containers)

	// Execute single parallel collection cycle
	collector.CollectOnce(ctx)

	// Verify maximum concurrent workers never exceeded 3
	if mc := maxConcurrent.Load(); mc > 3 {
		t.Fatalf("concurrency limit exceeded: expected max 3 concurrent requests, observed %d", mc)
	}

	samples := sink.GetSamples()
	// 12 containers * 3 baseline metrics = 36 samples
	if len(samples) != 36 {
		t.Fatalf("expected 36 metric samples, got %d", len(samples))
	}

	// Integration test with Ingestor and RollupEngine
	dbDir := t.TempDir()
	store, err := db.Open(ctx, dbDir)
	if err != nil {
		t.Fatalf("db.Open failed: %v", err)
	}
	defer store.Close()

	ingestor, err := metrics.NewIngestor(store.Metrics, metrics.IngestOptions{
		MaxBatchSize:  100,
		FlushInterval: 50 * time.Millisecond,
		QueueCapacity: 500,
		RingCapacity:  60,
	})
	if err != nil {
		t.Fatalf("metrics.NewIngestor failed: %v", err)
	}
	defer func() { _ = ingestor.Close(ctx) }()

	// Push collector samples into ingestor
	ingestor.Push(samples...)

	// Allow flush to complete into metrics.db
	time.Sleep(150 * time.Millisecond)

	// Verify HotCache has the container data
	latest := ingestor.Cache().GetEntityLatest("container:service-00")
	if latest == nil {
		t.Fatalf("expected container:service-00 in HotCache")
	}
	if cpuPoint, ok := latest["cpu_pct"]; !ok || cpuPoint.Value <= 0 {
		t.Errorf("expected positive cpu_pct in HotCache, got %v", cpuPoint)
	}

	// Run RollupEngine to verify automated 1d daily rollups
	rollup := metrics.NewRollupEngine(store.Metrics, nil)
	statsList, err := rollup.QueryTodaySummary(ctx, "container:service-00")
	if err != nil {
		t.Fatalf("QueryTodaySummary failed: %v", err)
	}

	foundCPU := false
	for _, st := range statsList {
		if st.Metric == "cpu_pct" {
			foundCPU = true
			if st.Avg <= 0 || st.Max <= 0 {
				t.Errorf("expected non-zero daily avg for container:service-00 cpu_pct, got: %+v", st)
			}
		}
	}
	if !foundCPU {
		t.Errorf("container:service-00 cpu_pct not found in today's daily summary: %+v", statsList)
	}
}
