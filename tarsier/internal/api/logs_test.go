package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dale-tomson/Tarsier/internal/db"
	"github.com/dale-tomson/Tarsier/internal/logs"
	"github.com/dale-tomson/Tarsier/internal/model"
	"github.com/klauspost/compress/zstd"
)

func setupLogsTestServer(t *testing.T, engine *logs.LogQueryEngine, tailCache *logs.TailCache) (*Server, *db.Store, *http.Cookie) {
	srv, store, cookie := setupMetricsTestServer(t, nil)
	srv.logEngine = engine
	srv.tailCache = tailCache
	return srv, store, cookie
}

func TestLogsAPI_SourcesAndUnfilteredTail(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

	tmpDir := t.TempDir()
	tailCache := logs.NewTailCache(10, 300)

	engine, err := logs.NewLogQueryEngine(logs.LogQueryEngineConfig{
		BaseDir:   tmpDir,
		TailCache: tailCache,
	})
	if err != nil {
		t.Fatalf("NewLogQueryEngine failed: %v", err)
	}

	srv, store, cookie := setupLogsTestServer(t, engine, tailCache)
	defer store.Close()

	// 1. Seed sources in config.db
	var s1ID, s2ID int64
	err = store.Config.ExecWrite(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(txCtx, `
			INSERT INTO source (kind, name, host_id, first_seen, last_seen)
			VALUES ('container', 'api', 1, 1700000000, 1700001000);
		`)
		if err != nil {
			return err
		}
		s1ID, _ = res.LastInsertId()

		res, err = tx.ExecContext(txCtx, `
			INSERT INTO source (kind, name, host_id, first_seen, last_seen)
			VALUES ('file', 'nginx', 1, 1700000000, 1700002000);
		`)
		if err != nil {
			return err
		}
		s2ID, _ = res.LastInsertId()

		_, err = tx.ExecContext(txCtx, `
			INSERT INTO container (id, name, image, first_seen, last_seen, last_status)
			VALUES ('c1', 'api', 'api:latest', 1700000000, 1700001000, 'running');
		`)
		return err
	})
	if err != nil {
		t.Fatalf("seed sources failed: %v", err)
	}

	// 2. Push 15 lines into TailCache for container:api
	for i := 1; i <= 15; i++ {
		tailCache.Push(model.LogEntry{
			Timestamp:       now.Add(time.Duration(i) * time.Second),
			TimestampSource: model.TimestampSourceLine,
			SourceID:        fmt.Sprintf("%d", s1ID),
			Level:           model.LevelInfo,
			LevelSource:     model.LevelSourceStructured,
			Stream:          model.StreamStdout,
			Message:         []byte(fmt.Sprintf("api log message %d", i)),
		})
	}

	ts := httptest.NewServer(srv.Router())
	defer ts.Close()

	client := ts.Client()

	// 3. Test GET /api/v1/sources
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/sources", nil)
	req.AddCookie(cookie)

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /api/v1/sources failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}

	var sourcesRes SourcesResponse
	if err := json.NewDecoder(resp.Body).Decode(&sourcesRes); err != nil {
		t.Fatalf("decode sources response failed: %v", err)
	}

	if len(sourcesRes.Sources) != 2 {
		t.Fatalf("expected 2 sources, got %d", len(sourcesRes.Sources))
	}

	sourceMap := make(map[string]SourceItem)
	for _, s := range sourcesRes.Sources {
		sourceMap[fmt.Sprintf("%s:%s", s.Kind, s.Name)] = s
	}

	apiSrc, exists := sourceMap["container:api"]
	if !exists {
		t.Fatalf("container:api not found in sources")
	}
	if apiSrc.BufferedLines != 15 {
		t.Errorf("expected 15 buffered lines for container:api, got %d", apiSrc.BufferedLines)
	}
	if apiSrc.Status != "running" {
		t.Errorf("expected status 'running' for container:api, got %q", apiSrc.Status)
	}

	nginxSrc, exists := sourceMap["file:nginx"]
	if !exists {
		t.Fatalf("file:nginx not found in sources")
	}
	if nginxSrc.BufferedLines != 0 {
		t.Errorf("expected 0 buffered lines for file:nginx, got %d", nginxSrc.BufferedLines)
	}

	// 4. Test GET /api/v1/logs?source=container:api
	reqLogs, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/logs?source=container:api", nil)
	reqLogs.AddCookie(cookie)

	respLogs, err := client.Do(reqLogs)
	if err != nil {
		t.Fatalf("GET /api/v1/logs failed: %v", err)
	}
	defer respLogs.Body.Close()

	if respLogs.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", respLogs.StatusCode)
	}

	var logsRes LogsQueryResponse
	if err := json.NewDecoder(respLogs.Body).Decode(&logsRes); err != nil {
		t.Fatalf("decode logs response failed: %v", err)
	}

	if !logsRes.FromCache {
		t.Errorf("expected from_cache == true")
	}
	if logsRes.ScannedBlocks != 0 {
		t.Errorf("expected scanned_blocks == 0, got %d", logsRes.ScannedBlocks)
	}

	// 4b. Test resolution with leading slash and bare name
	for _, qSource := range []string{"container:/api", "api"} {
		reqSlash, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/logs?source="+qSource, nil)
		reqSlash.AddCookie(cookie)
		respSlash, sErr := client.Do(reqSlash)
		if sErr != nil {
			t.Fatalf("GET /api/v1/logs?source=%s failed: %v", qSource, sErr)
		}
		if respSlash.StatusCode != http.StatusOK {
			t.Errorf("expected 200 OK for source=%s, got %d", qSource, respSlash.StatusCode)
		}
		respSlash.Body.Close()
	}
	if len(logsRes.Entries) != 15 {
		t.Fatalf("expected 15 entries, got %d", len(logsRes.Entries))
	}

	// Verify newest-first order
	if logsRes.Entries[0].Line != "api log message 15" {
		t.Errorf("expected first line 'api log message 15', got %q", logsRes.Entries[0].Line)
	}
	if logsRes.Entries[14].Line != "api log message 1" {
		t.Errorf("expected last line 'api log message 1', got %q", logsRes.Entries[14].Line)
	}

	// Verify metadata fields
	if logsRes.Entries[0].TSSrc != 1 {
		t.Errorf("expected ts_src == 1, got %d", logsRes.Entries[0].TSSrc)
	}
	if logsRes.Entries[0].LevelSrc != 1 {
		t.Errorf("expected level_src == 1, got %d", logsRes.Entries[0].LevelSrc)
	}
	if logsRes.Entries[0].Level != "info" {
		t.Errorf("expected level 'info', got %q", logsRes.Entries[0].Level)
	}

	// 5. Test numeric query GET /api/v1/logs?source=2 (file:nginx) -> empty
	reqNginx, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/api/v1/logs?source=%d", ts.URL, s2ID), nil)
	reqNginx.AddCookie(cookie)

	respNginx, err := client.Do(reqNginx)
	if err != nil {
		t.Fatalf("GET /api/v1/logs?source=%d failed: %v", s2ID, err)
	}
	defer respNginx.Body.Close()

	var nginxLogsRes LogsQueryResponse
	if err := json.NewDecoder(respNginx.Body).Decode(&nginxLogsRes); err != nil {
		t.Fatalf("decode nginx logs response failed: %v", err)
	}
	if len(nginxLogsRes.Entries) != 0 {
		t.Errorf("expected 0 entries for nginx, got %d", len(nginxLogsRes.Entries))
	}
}

func TestLogsAPI_SearchFilterAndCursorPagination(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 15, 14, 0, 0, 0, time.UTC)
	tmpDir := t.TempDir()

	idxMgr := logs.NewIndexManager(tmpDir)
	defer idxMgr.Close()

	chunkCfg := logs.ChunkWriterConfig{
		BaseDir:          tmpDir,
		BlockTargetBytes: 4 * 1024,
		FlushInterval:    1 * time.Second,
		ZstdLevel:        zstd.SpeedFastest,
		OnBlockFlushed: func(meta logs.BlockMeta) {
			_ = idxMgr.AppendBlock(meta)
		},
	}

	chunkMgr, err := logs.NewChunkWriterManager(chunkCfg)
	if err != nil {
		t.Fatalf("NewChunkWriterManager failed: %v", err)
	}

	// Write 500 lines with 12 errors containing "timeout"
	var totalTimeoutErrors int
	for i := 1; i <= 500; i++ {
		lvl := model.LevelInfo
		msg := fmt.Sprintf("service regular log line %04d", i)
		if i%40 == 0 {
			lvl = model.LevelError
			msg = fmt.Sprintf("database connection timeout error %04d", i)
			totalTimeoutErrors++
		}

		_ = chunkMgr.Write(model.LogEntry{
			Timestamp: now.Add(time.Duration(i) * time.Millisecond),
			SourceID:  "1",
			Level:     lvl,
			Stream:    model.StreamStderr,
			Message:   []byte(msg),
		})
	}

	_ = chunkMgr.Close(context.Background())
	_ = idxMgr.Sync()

	engine, err := logs.NewLogQueryEngine(logs.LogQueryEngineConfig{
		BaseDir: tmpDir,
	})
	if err != nil {
		t.Fatalf("NewLogQueryEngine failed: %v", err)
	}

	srv, store, cookie := setupLogsTestServer(t, engine, nil)
	defer store.Close()

	// Seed source 1 in config.db
	_ = store.Config.ExecWrite(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(txCtx, `INSERT INTO source (id, kind, name, first_seen, last_seen) VALUES (1, 'file', 'app.log', 1700000000, 1700001000);`)
		return err
	})

	ts := httptest.NewServer(srv.Router())
	defer ts.Close()
	client := ts.Client()

	// 1. Paginate with limit=5, level=error, q=timeout
	var collected []LogEntryItem
	cursor := ""

	for {
		url := fmt.Sprintf("%s/api/v1/logs?source=1&level=error&q=timeout&limit=5", ts.URL)
		if cursor != "" {
			url += "&cursor=" + cursor
		}

		req, _ := http.NewRequest(http.MethodGet, url, nil)
		req.AddCookie(cookie)

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET /api/v1/logs page failed: %v", err)
		}

		var pageRes LogsQueryResponse
		_ = json.NewDecoder(resp.Body).Decode(&pageRes)
		resp.Body.Close()

		collected = append(collected, pageRes.Entries...)
		if !pageRes.HasMore || pageRes.NextCursor == "" {
			break
		}
		cursor = pageRes.NextCursor
	}

	if len(collected) != totalTimeoutErrors {
		t.Fatalf("collected %d timeout error entries, expected %d", len(collected), totalTimeoutErrors)
	}

	// Verify no duplicates
	seenIDs := make(map[string]bool)
	for _, ent := range collected {
		if seenIDs[ent.ID] {
			t.Fatalf("duplicate ID %s in paginated results", ent.ID)
		}
		seenIDs[ent.ID] = true
	}

	// 2. Test missing source param returns 400
	reqMissing, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/logs", nil)
	reqMissing.AddCookie(cookie)
	respMissing, _ := client.Do(reqMissing)
	if respMissing.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for missing source, got %d", respMissing.StatusCode)
	}

	// 3. Test invalid regex returns 400
	reqRegex, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/logs?source=1&regex=true&q=[unclosed", nil)
	reqRegex.AddCookie(cookie)
	respRegex, _ := client.Do(reqRegex)
	if respRegex.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for invalid regex, got %d", respRegex.StatusCode)
	}

	// 4. Test nonexistent source returns 404
	req404, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/logs?source=9999", nil)
	req404.AddCookie(cookie)
	resp404, _ := client.Do(req404)
	if resp404.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 Not Found for nonexistent source, got %d", resp404.StatusCode)
	}
}

func TestLogsAPI_ContextEndpoint(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 15, 14, 0, 0, 0, time.UTC)
	tmpDir := t.TempDir()

	idxMgr := logs.NewIndexManager(tmpDir)
	defer idxMgr.Close()

	chunkCfg := logs.ChunkWriterConfig{
		BaseDir:          tmpDir,
		BlockTargetBytes: 1024,
		FlushInterval:    1 * time.Second,
		ZstdLevel:        zstd.SpeedFastest,
		OnBlockFlushed: func(meta logs.BlockMeta) {
			_ = idxMgr.AppendBlock(meta)
		},
	}

	chunkMgr, err := logs.NewChunkWriterManager(chunkCfg)
	if err != nil {
		t.Fatalf("NewChunkWriterManager failed: %v", err)
	}

	for i := 1; i <= 100; i++ {
		_ = chunkMgr.Write(model.LogEntry{
			Timestamp: now.Add(time.Duration(i) * time.Millisecond),
			SourceID:  "1",
			Level:     model.LevelInfo,
			Message:   []byte(fmt.Sprintf("context message line %03d", i)),
		})
	}
	_ = chunkMgr.Close(context.Background())
	_ = idxMgr.Sync()

	engine, err := logs.NewLogQueryEngine(logs.LogQueryEngineConfig{
		BaseDir: tmpDir,
	})
	if err != nil {
		t.Fatalf("NewLogQueryEngine failed: %v", err)
	}

	srv, store, cookie := setupLogsTestServer(t, engine, nil)
	defer store.Close()

	_ = store.Config.ExecWrite(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(txCtx, `INSERT INTO source (id, kind, name, first_seen, last_seen) VALUES (1, 'file', 'app.log', 1700000000, 1700001000);`)
		return err
	})

	ts := httptest.NewServer(srv.Router())
	defer ts.Close()
	client := ts.Client()

	// 1. Get an entry ID from /api/v1/logs
	reqLogs, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/logs?source=1&limit=10", nil)
	reqLogs.AddCookie(cookie)
	respLogs, err := client.Do(reqLogs)
	if err != nil {
		t.Fatalf("GET /api/v1/logs failed: %v", err)
	}

	var logsRes LogsQueryResponse
	_ = json.NewDecoder(respLogs.Body).Decode(&logsRes)
	respLogs.Body.Close()

	if len(logsRes.Entries) == 0 {
		t.Fatalf("no entries returned")
	}
	targetID := logsRes.Entries[5].ID

	// 2. Query /api/v1/logs/context?source=1&id=<targetID>&before=3&after=3
	contextURL := fmt.Sprintf("%s/api/v1/logs/context?source=1&id=%s&before=3&after=3", ts.URL, targetID)
	reqCtx, _ := http.NewRequest(http.MethodGet, contextURL, nil)
	reqCtx.AddCookie(cookie)

	respCtx, err := client.Do(reqCtx)
	if err != nil {
		t.Fatalf("GET /api/v1/logs/context failed: %v", err)
	}
	defer respCtx.Body.Close()

	if respCtx.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", respCtx.StatusCode)
	}

	var ctxRes LogsContextResponse
	if err := json.NewDecoder(respCtx.Body).Decode(&ctxRes); err != nil {
		t.Fatalf("decode context response failed: %v", err)
	}

	if len(ctxRes.Entries) != 7 {
		t.Fatalf("expected 7 entries, got %d", len(ctxRes.Entries))
	}
	if ctxRes.TargetIndex != 3 {
		t.Errorf("expected target_index == 3, got %d", ctxRes.TargetIndex)
	}
	if ctxRes.Entries[ctxRes.TargetIndex].ID != targetID {
		t.Errorf("expected target entry ID %s, got %s", targetID, ctxRes.Entries[ctxRes.TargetIndex].ID)
	}
}

func TestLogsAPI_LatestFirstOrdering(t *testing.T) {
	ctx := context.Background()
	baseTime := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

	tmpDir := t.TempDir()
	tailCache := logs.NewTailCache(10, 300)

	engine, err := logs.NewLogQueryEngine(logs.LogQueryEngineConfig{
		BaseDir:   tmpDir,
		TailCache: tailCache,
	})
	if err != nil {
		t.Fatalf("NewLogQueryEngine failed: %v", err)
	}

	srv, store, cookie := setupLogsTestServer(t, engine, tailCache)
	defer store.Close()

	var sID int64
	err = store.Config.ExecWrite(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(txCtx, `
			INSERT INTO source (kind, name, host_id, first_seen, last_seen)
			VALUES ('container', 'order-test', 1, 1700000000, 1700001000);
		`)
		if err != nil {
			return err
		}
		sID, _ = res.LastInsertId()
		return nil
	})
	if err != nil {
		t.Fatalf("seed source failed: %v", err)
	}

	// Push entries with intentionally non-sequential timestamps
	// Timestamps in seconds offset: +10, +50, +30, +20, +40
	offsets := []int{10, 50, 30, 20, 40}
	for i, off := range offsets {
		tailCache.Push(model.LogEntry{
			Timestamp:       baseTime.Add(time.Duration(off) * time.Second),
			TimestampSource: model.TimestampSourceLine,
			SourceID:        fmt.Sprintf("%d", sID),
			Level:           model.LevelInfo,
			LevelSource:     model.LevelSourceStructured,
			Stream:          model.StreamStdout,
			Message:         []byte(fmt.Sprintf("msg offset %d", off)),
		})
		_ = i
	}

	ts := httptest.NewServer(srv.Router())
	defer ts.Close()

	client := ts.Client()
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/api/v1/logs?source=%d", ts.URL, sID), nil)
	req.AddCookie(cookie)

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /api/v1/logs failed: %v", err)
	}
	defer resp.Body.Close()

	var res LogsQueryResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatalf("decode failed: %v", err)
	}

	if len(res.Entries) != 5 {
		t.Fatalf("expected 5 entries, got %d", len(res.Entries))
	}

	// Verify strictly descending timestamps (latest first: 50, 40, 30, 20, 10)
	expectedOffsets := []int{50, 40, 30, 20, 10}
	for idx, expOff := range expectedOffsets {
		expLine := fmt.Sprintf("msg offset %d", expOff)
		if res.Entries[idx].Line != expLine {
			t.Errorf("entry %d: expected %q, got %q", idx, expLine, res.Entries[idx].Line)
		}
	}
}

func TestLogsAPI_TimeRangeFiltering(t *testing.T) {
	ctx := context.Background()
	baseTime := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

	// Test parseLogTimeParam directly with various formats
	rfcStr := baseTime.Format(time.RFC3339)
	ts1, err := parseLogTimeParam(rfcStr)
	if err != nil || ts1 != baseTime.UnixNano() {
		t.Fatalf("parseLogTimeParam RFC3339 failed: ts=%d err=%v", ts1, err)
	}

	dtLocalStr := "2026-11-16T12:00"
	ts2, err := parseLogTimeParam(dtLocalStr)
	if err != nil || ts2 != baseTime.UnixNano() {
		t.Fatalf("parseLogTimeParam datetime-local failed: ts=%d err=%v", ts2, err)
	}

	relTs, err := parseLogTimeParam("1h")
	if err != nil || relTs <= 0 {
		t.Fatalf("parseLogTimeParam relative '1h' failed: ts=%d err=%v", relTs, err)
	}

	// Test HTTP query with from & to
	tmpDir := t.TempDir()
	tailCache := logs.NewTailCache(10, 300)
	engine, err := logs.NewLogQueryEngine(logs.LogQueryEngineConfig{
		BaseDir:   tmpDir,
		TailCache: tailCache,
	})
	if err != nil {
		t.Fatalf("NewLogQueryEngine failed: %v", err)
	}

	srv, store, cookie := setupLogsTestServer(t, engine, tailCache)
	defer store.Close()

	var sID int64
	err = store.Config.ExecWrite(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(txCtx, `
			INSERT INTO source (kind, name, host_id, first_seen, last_seen)
			VALUES ('container', 'timerange-test', 1, 1700000000, 1700001000);
		`)
		if err != nil {
			return err
		}
		sID, _ = res.LastInsertId()
		return nil
	})
	if err != nil {
		t.Fatalf("seed source failed: %v", err)
	}

	// Push logs at +10s, +20s, +30s, +40s, +50s
	offsets := []int{10, 20, 30, 40, 50}
	for _, off := range offsets {
		tailCache.Push(model.LogEntry{
			Timestamp:       baseTime.Add(time.Duration(off) * time.Second),
			TimestampSource: model.TimestampSourceLine,
			SourceID:        fmt.Sprintf("%d", sID),
			Level:           model.LevelInfo,
			LevelSource:     model.LevelSourceStructured,
			Stream:          model.StreamStdout,
			Message:         []byte(fmt.Sprintf("log at %d", off)),
		})
	}

	ts := httptest.NewServer(srv.Router())
	defer ts.Close()

	client := ts.Client()

	// Query from +15s to +35s (should match +20s and +30s only)
	fromParam := baseTime.Add(15 * time.Second).Format(time.RFC3339)
	toParam := baseTime.Add(35 * time.Second).Format(time.RFC3339)
	reqUrl := fmt.Sprintf("%s/api/v1/logs?source=%d&from=%s&to=%s", ts.URL, sID, fromParam, toParam)

	req, _ := http.NewRequest(http.MethodGet, reqUrl, nil)
	req.AddCookie(cookie)

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /api/v1/logs with time range failed: %v", err)
	}
	defer resp.Body.Close()

	var res LogsQueryResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatalf("decode failed: %v", err)
	}

	if len(res.Entries) != 2 {
		t.Fatalf("expected exactly 2 entries in time range, got %d", len(res.Entries))
	}

	// Ordered latest-first: +30s then +20s
	if res.Entries[0].Line != "log at 30" || res.Entries[1].Line != "log at 20" {
		t.Errorf("unexpected entries: %+v", res.Entries)
	}
}
