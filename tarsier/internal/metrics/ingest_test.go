package metrics_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/dale-tomson/Tarsier/internal/db"
	"github.com/dale-tomson/Tarsier/internal/metrics"
	"github.com/dale-tomson/Tarsier/internal/model"
)

func TestSeriesRegistry_CacheAndPersist(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	store, err := db.Open(ctx, tmpDir)
	if err != nil {
		t.Fatalf("db.Open failed: %v", err)
	}
	defer store.Close()

	// Configure small max series cap to verify limit enforcement
	reg := metrics.NewSeriesRegistry(store.Metrics, 3)

	id1, err := reg.GetOrCreate(ctx, "host", "cpu_pct")
	if err != nil {
		t.Fatalf("GetOrCreate failed: %v", err)
	}
	if id1 <= 0 {
		t.Fatalf("expected positive series id, got %d", id1)
	}

	// Repeated lookup must return identical id
	id1Repeat, err := reg.GetOrCreate(ctx, "host", "cpu_pct")
	if err != nil || id1Repeat != id1 {
		t.Fatalf("expected cached id %d, got %d (err: %v)", id1, id1Repeat, err)
	}

	id2, err := reg.GetOrCreate(ctx, "host", "mem_used")
	if err != nil || id2 == id1 {
		t.Fatalf("expected distinct id for mem_used, got %d", id2)
	}

	id3, err := reg.GetOrCreate(ctx, "disk:/", "disk_used_pct")
	if err != nil || id3 <= 0 {
		t.Fatalf("expected id for disk_used_pct, got %d", id3)
	}

	// 4th series must exceed maxSeries cap of 3
	_, err = reg.GetOrCreate(ctx, "disk:/mnt", "disk_used_pct")
	if err != metrics.ErrMaxSeriesExceeded {
		t.Fatalf("expected ErrMaxSeriesExceeded, got: %v", err)
	}

	// Verify persistence in SQLite
	var count int
	if err := store.Metrics.RO.QueryRowContext(ctx, "SELECT COUNT(*) FROM series;").Scan(&count); err != nil {
		t.Fatalf("count series in DB failed: %v", err)
	}
	if count != 3 {
		t.Fatalf("expected 3 persisted series rows, got %d", count)
	}

	// Warmup test: new registry instance reading existing DB
	reg2 := metrics.NewSeriesRegistry(store.Metrics, 100)
	if err := reg2.LoadSeries(ctx); err != nil {
		t.Fatalf("LoadSeries failed: %v", err)
	}
	if reg2.Count() != 3 {
		t.Fatalf("expected 3 loaded series in warmed registry, got %d", reg2.Count())
	}

	cachedID, ok := reg2.Lookup("host", "cpu_pct")
	if !ok || cachedID != id1 {
		t.Fatalf("expected loaded id %d, got %d (ok=%v)", id1, cachedID, ok)
	}
}

func TestIngestor_BatchFlushAndIdempotency(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	store, err := db.Open(ctx, tmpDir)
	if err != nil {
		t.Fatalf("db.Open failed: %v", err)
	}
	defer store.Close()

	ing, err := metrics.NewIngestor(store.Metrics, metrics.IngestOptions{
		MaxBatchSize:  500,
		FlushInterval: 100 * time.Millisecond,
		QueueCapacity: 4096,
	})
	if err != nil {
		t.Fatalf("NewIngestor failed: %v", err)
	}

	// Push 1,250 samples across 5 series (250 ticks per series)
	totalSamples := 1250
	samples := make([]model.MetricSample, 0, totalSamples)
	for i := 0; i < 250; i++ {
		ts := int64(1731650000 + i*10)
		for s := 0; s < 5; s++ {
			samples = append(samples, model.MetricSample{
				Entity: fmt.Sprintf("service-%d", s),
				Metric: "cpu_pct",
				TS:     ts,
				Value:  float64(i % 100),
			})
		}
	}

	accepted := ing.Push(samples...)
	if accepted != totalSamples {
		t.Fatalf("expected %d accepted samples, got %d", totalSamples, accepted)
	}

	// Also verify samples exist in HotCache immediately
	pt, ok := ing.Cache().GetLatest("service-0", "cpu_pct")
	if !ok || pt.TS != int64(1731650000+249*10) {
		t.Fatalf("expected immediate HotCache availability, got %+v (ok=%v)", pt, ok)
	}

	// Close to ensure all batches flush cleanly to SQLite
	if err := ing.Close(ctx); err != nil {
		t.Fatalf("Ingestor.Close failed: %v", err)
	}

	var rawCount int
	if err := store.Metrics.RO.QueryRowContext(ctx, "SELECT COUNT(*) FROM sample_raw;").Scan(&rawCount); err != nil {
		t.Fatalf("count sample_raw failed: %v", err)
	}
	if rawCount != totalSamples {
		t.Fatalf("expected %d rows in sample_raw, got %d", totalSamples, rawCount)
	}

	// Idempotency: re-push identical samples with updated values into a new Ingestor instance
	ing2, err := metrics.NewIngestor(store.Metrics, metrics.IngestOptions{
		MaxBatchSize:  500,
		FlushInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewIngestor 2 failed: %v", err)
	}

	for i := range samples {
		samples[i].Value += 1.0 // update value
	}
	ing2.Push(samples...)

	if err := ing2.Close(ctx); err != nil {
		t.Fatalf("Ingestor 2 Close failed: %v", err)
	}

	// Count must remain 1250 (updated in-place via INSERT OR REPLACE WITHOUT ROWID)
	if err := store.Metrics.RO.QueryRowContext(ctx, "SELECT COUNT(*) FROM sample_raw;").Scan(&rawCount); err != nil {
		t.Fatalf("count sample_raw after update failed: %v", err)
	}
	if rawCount != totalSamples {
		t.Fatalf("expected count to remain %d after updates, got %d", totalSamples, rawCount)
	}
}

func TestIngestor_GracefulShutdownFlush(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	store, err := db.Open(ctx, tmpDir)
	if err != nil {
		t.Fatalf("db.Open failed: %v", err)
	}
	defer store.Close()

	ing, err := metrics.NewIngestor(store.Metrics, metrics.IngestOptions{
		MaxBatchSize:  500,
		FlushInterval: 10 * time.Second, // Long flush interval
		QueueCapacity: 1000,
	})
	if err != nil {
		t.Fatalf("NewIngestor failed: %v", err)
	}

	// Push 50 samples (well below 500 MaxBatchSize)
	for i := 0; i < 50; i++ {
		ing.Push(model.MetricSample{
			Entity: "host",
			Metric: "cpu_pct",
			TS:     int64(1731650000 + i*10),
			Value:  float64(i),
		})
	}

	// Immediately close without waiting for the 10s timer
	if err := ing.Close(ctx); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	var count int
	if err := store.Metrics.RO.QueryRowContext(ctx, "SELECT COUNT(*) FROM sample_raw;").Scan(&count); err != nil {
		t.Fatalf("query sample_raw failed: %v", err)
	}
	if count != 50 {
		t.Fatalf("expected all 50 samples flushed on Close, got %d", count)
	}
}

func BenchmarkIngestor_5000Samples(b *testing.B) {
	ctx := context.Background()
	tmpDir := b.TempDir()
	store, err := db.Open(ctx, tmpDir)
	if err != nil {
		b.Fatalf("db.Open failed: %v", err)
	}
	defer store.Close()

	// Prepare 5,000 samples across 50 series
	samples := make([]model.MetricSample, 5000)
	for i := 0; i < 5000; i++ {
		samples[i] = model.MetricSample{
			Entity: fmt.Sprintf("entity-%d", i%50),
			Metric: "cpu_pct",
			TS:     int64(1731650000 + (i/50)*10),
			Value:  float64(i % 100),
		}
	}

	b.ResetTimer()
	b.ReportAllocs()

	for n := 0; n < b.N; n++ {
		ing, err := metrics.NewIngestor(store.Metrics, metrics.IngestOptions{
			MaxBatchSize:  500,
			FlushInterval: 50 * time.Millisecond,
			QueueCapacity: 10000,
		})
		if err != nil {
			b.Fatalf("NewIngestor failed: %v", err)
		}

		ing.Push(samples...)

		if err := ing.Close(ctx); err != nil {
			b.Fatalf("Close failed: %v", err)
		}
	}
}
