package alert

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dale-tomson/Tarsier/internal/config"
)

func TestHeartbeatWorker_StartupAndTickerCadence(t *testing.T) {
	var pingCount atomic.Int32
	var capturedUserAgent atomic.Value

	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		pingCount.Add(1)
		capturedUserAgent.Store(req.Header.Get("User-Agent"))
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("OK")),
			Header:     make(http.Header),
		}, nil
	})

	httpClient := &http.Client{Transport: transport}
	cfg := config.HeartbeatConfig{
		Enabled: true,
		URL:     "http://hc.test.local/ping/12345",
	}

	worker, err := NewHeartbeatWorker(cfg,
		WithHeartbeatHTTPClient(httpClient),
		WithHeartbeatVersion("2.1"),
		WithHeartbeatInterval(30*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("NewHeartbeatWorker: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		worker.Run(ctx)
		close(done)
	}()

	// Run for 120ms (startup ping at 0ms, ticks at ~30ms, ~60ms, ~90ms -> at least 3 pings)
	time.Sleep(120 * time.Millisecond)
	cancel()
	<-done

	count := pingCount.Load()
	if count < 3 {
		t.Fatalf("expected at least 3 pings, got %d", count)
	}

	ua, _ := capturedUserAgent.Load().(string)
	if ua != "Tarsier/2.1" {
		t.Errorf("expected User-Agent 'Tarsier/2.1', got %q", ua)
	}

	st := worker.Status()
	if st.LastSuccessAt == 0 {
		t.Error("expected LastSuccessAt > 0")
	}
	if st.ConsecutiveErr != 0 {
		t.Errorf("expected ConsecutiveErr 0, got %d", st.ConsecutiveErr)
	}
	if st.TargetHost != "hc.test.local" {
		t.Errorf("expected TargetHost 'hc.test.local', got %q", st.TargetHost)
	}
}

func TestHeartbeatWorker_FailureIsolationAndSecretRedaction(t *testing.T) {
	secretToken := "super-secret-uuid-token-xyz"
	targetURL := "http://hc.test.local/ping/" + secretToken
	var callCount atomic.Int32

	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		count := callCount.Add(1)
		// Return 500 twice, then 200 OK
		if count <= 2 {
			return &http.Response{
				StatusCode: http.StatusInternalServerError,
				Body:       io.NopCloser(strings.NewReader("upstream failed: " + targetURL)),
				Header:     make(http.Header),
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("OK")),
			Header:     make(http.Header),
		}, nil
	})

	httpClient := &http.Client{Transport: transport}
	cfg := config.HeartbeatConfig{
		Enabled: true,
		URL:     targetURL,
	}

	worker, err := NewHeartbeatWorker(cfg,
		WithHeartbeatHTTPClient(httpClient),
		WithHeartbeatInterval(25*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("NewHeartbeatWorker: %v", err)
	}

	ctx := context.Background()

	// Attempt 1 -> 500
	_ = worker.PingOnce(ctx)
	st1 := worker.Status()
	if st1.ConsecutiveErr != 1 {
		t.Errorf("expected ConsecutiveErr 1, got %d", st1.ConsecutiveErr)
	}
	if strings.Contains(st1.LastError, secretToken) {
		t.Errorf("secret token leaked in LastError: %s", st1.LastError)
	}
	if strings.Contains(st1.TargetHost, secretToken) {
		t.Errorf("secret token leaked in TargetHost: %s", st1.TargetHost)
	}

	// Attempt 2 -> 500
	_ = worker.PingOnce(ctx)
	st2 := worker.Status()
	if st2.ConsecutiveErr != 2 {
		t.Errorf("expected ConsecutiveErr 2, got %d", st2.ConsecutiveErr)
	}

	// Attempt 3 -> 200 OK
	_ = worker.PingOnce(ctx)
	st3 := worker.Status()
	if st3.ConsecutiveErr != 0 {
		t.Errorf("expected ConsecutiveErr reset to 0 after success, got %d", st3.ConsecutiveErr)
	}
	if st3.LastStatusCode != 200 {
		t.Errorf("expected LastStatusCode 200, got %d", st3.LastStatusCode)
	}
	if st3.LastError != "" {
		t.Errorf("expected empty LastError after success, got %q", st3.LastError)
	}
}

func TestHeartbeatWorker_HealthGateAndHotReload(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(false)

	var pings atomic.Int32
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		pings.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("OK")),
			Header:     make(http.Header),
		}, nil
	})

	httpClient := &http.Client{Transport: transport}
	cfg := config.HeartbeatConfig{
		Enabled: true,
		URL:     "http://hc.test.local/ping/alive",
	}

	worker, err := NewHeartbeatWorker(cfg,
		WithHeartbeatHTTPClient(httpClient),
		WithHealthCheck(healthy.Load),
		WithHeartbeatInterval(20*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("NewHeartbeatWorker: %v", err)
	}

	ctx := context.Background()

	// 1. Health check returns false -> ping must be suppressed
	err = worker.PingOnce(ctx)
	if err == nil {
		t.Fatal("expected error when health check fails, got nil")
	}
	if pings.Load() != 0 {
		t.Fatalf("pings should not be sent while unhealthy, got %d", pings.Load())
	}

	// 2. Flip health check to true -> ping succeeds
	worker.healthCheck = func() bool { return true }
	err = worker.PingOnce(ctx)
	if err != nil {
		t.Fatalf("expected ping success, got %v", err)
	}
	if pings.Load() != 1 {
		t.Fatalf("expected 1 ping after healthy, got %d", pings.Load())
	}

	// 3. Hot reload to disabled
	err = worker.Reload(config.HeartbeatConfig{
		Enabled: false,
	})
	if err != nil {
		t.Fatalf("Reload failed: %v", err)
	}

	if worker.Status().Enabled {
		t.Error("expected status Enabled to be false after reload")
	}

	// Calling PingOnce when disabled does nothing
	_ = worker.PingOnce(ctx)
	if pings.Load() != 1 {
		t.Errorf("pings should not increment when disabled, got %d", pings.Load())
	}
}
