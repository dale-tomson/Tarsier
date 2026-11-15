package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dale-tomson/Tarsier/internal/auth"
	"github.com/dale-tomson/Tarsier/internal/logs"
	"github.com/dale-tomson/Tarsier/internal/model"
)

// SourceItem represents a single registered log source for GET /api/v1/sources.
type SourceItem struct {
	ID            int64  `json:"id"`
	Kind          string `json:"kind"`
	Name          string `json:"name"`
	LastSeen      string `json:"last_seen"` // RFC 3339
	BufferedLines int    `json:"buffered_lines"`
}

// SourcesResponse is the payload returned by GET /api/v1/sources.
type SourcesResponse struct {
	Sources []SourceItem `json:"sources"`
}

// SourceMeta represents the resolved source metadata.
type SourceMeta struct {
	ID   int64  `json:"id"`
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// LogEntryItem represents a single formatted log line in GET /api/v1/logs.
type LogEntryItem struct {
	ID       string `json:"id"`
	TS       string `json:"ts"` // RFC 3339 with nanoseconds
	TSSrc    uint8  `json:"ts_src"`
	Level    string `json:"level"`
	LevelSrc uint8  `json:"level_src"`
	Stream   string `json:"stream"`
	Line     string `json:"line"`
}

// LogsQueryResponse is the payload returned by GET /api/v1/logs.
type LogsQueryResponse struct {
	Source        SourceMeta     `json:"source"`
	FromCache     bool           `json:"from_cache"`
	ScannedBlocks int            `json:"scanned_blocks"`
	HasMore       bool           `json:"has_more"`
	NextCursor    string         `json:"next_cursor"`
	Entries       []LogEntryItem `json:"entries"`
}

// LogsContextResponse is the payload returned by GET /api/v1/logs/context.
type LogsContextResponse struct {
	TargetIndex int            `json:"target_index"`
	Entries     []LogEntryItem `json:"entries"`
}

// handleSourcesList handles GET /api/v1/sources.
func (s *Server) handleSourcesList(w http.ResponseWriter, r *http.Request) {
	sources := make([]SourceItem, 0)

	if s.configDB != nil {
		rows, err := s.configDB.RO.QueryContext(r.Context(),
			"SELECT id, kind, name, last_seen FROM source ORDER BY kind ASC, name ASC")
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var item SourceItem
				var lastSeenSec int64
				if scanErr := rows.Scan(&item.ID, &item.Kind, &item.Name, &lastSeenSec); scanErr == nil {
					if lastSeenSec > 0 {
						item.LastSeen = time.Unix(lastSeenSec, 0).UTC().Format(time.RFC3339)
					}
					if s.tailCache != nil {
						item.BufferedLines = s.tailCache.BufferedLines(item.ID)
					}
					sources = append(sources, item)
				}
			}
		}
	}

	auth.WriteJSON(w, http.StatusOK, SourcesResponse{Sources: sources})
}

// handleLogsQuery handles GET /api/v1/logs.
func (s *Server) handleLogsQuery(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	sourceParam := strings.TrimSpace(q.Get("source"))
	if sourceParam == "" {
		auth.WriteJSONError(w, http.StatusBadRequest, "bad_request", "source parameter is required")
		return
	}

	srcMeta, err := s.resolveSource(r.Context(), sourceParam)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			auth.WriteJSONError(w, http.StatusNotFound, "not_found", "source not found")
			return
		}
		auth.WriteJSONError(w, http.StatusInternalServerError, "query_error", err.Error())
		return
	}

	var isRegex bool
	if regexStr := q.Get("regex"); regexStr != "" {
		isRegex, _ = strconv.ParseBool(regexStr)
	}

	queryPattern := q.Get("q")
	if len(queryPattern) > 512 {
		auth.WriteJSONError(w, http.StatusBadRequest, "invalid_query", "query pattern exceeds 512 characters")
		return
	}

	var minLvl model.Level
	if lvlParam := strings.TrimSpace(q.Get("level")); lvlParam != "" {
		if num, err := strconv.Atoi(lvlParam); err == nil && num >= 0 && num <= 5 {
			minLvl = model.Level(num)
		} else {
			minLvl = model.ParseLevel(lvlParam)
		}
	}

	var stream model.Stream
	if streamParam := strings.TrimSpace(strings.ToLower(q.Get("stream"))); streamParam != "" {
		switch streamParam {
		case "stdout", "1":
			stream = model.StreamStdout
		case "stderr", "2":
			stream = model.StreamStderr
		}
	}

	fromTS, err := parseLogTimeParam(q.Get("from"))
	if err != nil {
		auth.WriteJSONError(w, http.StatusBadRequest, "invalid_param", "invalid from timestamp")
		return
	}

	toTS, err := parseLogTimeParam(q.Get("to"))
	if err != nil {
		auth.WriteJSONError(w, http.StatusBadRequest, "invalid_param", "invalid to timestamp")
		return
	}

	limit := 200
	if limitParam := q.Get("limit"); limitParam != "" {
		if l, err := strconv.Atoi(limitParam); err == nil {
			limit = l
		}
	}

	cursor := q.Get("cursor")

	if s.logEngine == nil {
		auth.WriteJSON(w, http.StatusOK, LogsQueryResponse{
			Source:     srcMeta,
			FromCache:  true,
			Entries:    make([]LogEntryItem, 0),
			NextCursor: "",
			HasMore:    false,
		})
		return
	}

	searchRes, err := s.logEngine.Search(r.Context(), logs.LogQuery{
		SourceID:   srcMeta.ID,
		Query:      queryPattern,
		Regex:      isRegex,
		IgnoreCase: true,
		MinLevel:   minLvl,
		Stream:     stream,
		FromTS:     fromTS,
		ToTS:       toTS,
		Cursor:     cursor,
		Limit:      limit,
	})
	if err != nil {
		if errors.Is(err, logs.ErrInvalidQuery) {
			auth.WriteJSONError(w, http.StatusBadRequest, "invalid_query", err.Error())
			return
		}
		auth.WriteJSONError(w, http.StatusInternalServerError, "search_error", err.Error())
		return
	}

	entries := make([]LogEntryItem, len(searchRes.Entries))
	for i, ent := range searchRes.Entries {
		streamStr := ""
		if ent.Stream == model.StreamStdout {
			streamStr = "stdout"
		} else if ent.Stream == model.StreamStderr {
			streamStr = "stderr"
		}

		entries[i] = LogEntryItem{
			ID:       ent.ID,
			TS:       time.Unix(0, ent.TS).UTC().Format(time.RFC3339Nano),
			TSSrc:    uint8(ent.TSSrc),
			Level:    strings.ToLower(ent.Level.String()),
			LevelSrc: uint8(ent.LevelSrc),
			Stream:   streamStr,
			Line:     ent.Line,
		}
	}

	auth.WriteJSON(w, http.StatusOK, LogsQueryResponse{
		Source:        srcMeta,
		FromCache:     searchRes.FromCache,
		ScannedBlocks: searchRes.ScannedBlocks,
		HasMore:       searchRes.HasMore,
		NextCursor:    searchRes.NextCursor,
		Entries:       entries,
	})
}

// handleLogsContext handles GET /api/v1/logs/context.
func (s *Server) handleLogsContext(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	sourceParam := strings.TrimSpace(q.Get("source"))
	if sourceParam == "" {
		auth.WriteJSONError(w, http.StatusBadRequest, "bad_request", "source parameter is required")
		return
	}

	recordID := strings.TrimSpace(q.Get("id"))
	if recordID == "" {
		auth.WriteJSONError(w, http.StatusBadRequest, "bad_request", "id parameter is required")
		return
	}

	srcMeta, err := s.resolveSource(r.Context(), sourceParam)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			auth.WriteJSONError(w, http.StatusNotFound, "not_found", "source not found")
			return
		}
		auth.WriteJSONError(w, http.StatusInternalServerError, "query_error", err.Error())
		return
	}

	before := 20
	if beforeParam := q.Get("before"); beforeParam != "" {
		if b, err := strconv.Atoi(beforeParam); err == nil {
			before = b
		}
	}

	after := 20
	if afterParam := q.Get("after"); afterParam != "" {
		if a, err := strconv.Atoi(afterParam); err == nil {
			after = a
		}
	}

	if s.logEngine == nil {
		auth.WriteJSONError(w, http.StatusNotFound, "not_found", "log record not found")
		return
	}

	records, targetIdx, err := s.logEngine.ReadContext(r.Context(), srcMeta.ID, recordID, before, after)
	if err != nil {
		if errors.Is(err, logs.ErrRecordNotFound) {
			auth.WriteJSONError(w, http.StatusNotFound, "not_found", "log record not found")
			return
		}
		auth.WriteJSONError(w, http.StatusInternalServerError, "context_error", err.Error())
		return
	}

	entries := make([]LogEntryItem, len(records))
	for i, ent := range records {
		streamStr := ""
		if ent.Stream == model.StreamStdout {
			streamStr = "stdout"
		} else if ent.Stream == model.StreamStderr {
			streamStr = "stderr"
		}

		entries[i] = LogEntryItem{
			ID:       ent.ID,
			TS:       time.Unix(0, ent.TS).UTC().Format(time.RFC3339Nano),
			TSSrc:    uint8(ent.TSSrc),
			Level:    strings.ToLower(ent.Level.String()),
			LevelSrc: uint8(ent.LevelSrc),
			Stream:   streamStr,
			Line:     ent.Line,
		}
	}

	auth.WriteJSON(w, http.StatusOK, LogsContextResponse{
		TargetIndex: targetIdx,
		Entries:     entries,
	})
}

// resolveSource resolves a numeric ID or kind:name or name string to SourceMeta.
func (s *Server) resolveSource(ctx context.Context, param string) (SourceMeta, error) {
	if s.configDB == nil {
		// In-memory fallback if configDB is nil
		if id, err := strconv.ParseInt(param, 10, 64); err == nil {
			return SourceMeta{ID: id, Kind: "file", Name: param}, nil
		}
		return SourceMeta{ID: 1, Kind: "file", Name: param}, nil
	}

	var meta SourceMeta

	// 1. If numeric ID
	if id, err := strconv.ParseInt(param, 10, 64); err == nil {
		err := s.configDB.RO.QueryRowContext(ctx,
			"SELECT id, kind, name FROM source WHERE id = ?", id).
			Scan(&meta.ID, &meta.Kind, &meta.Name)
		return meta, err
	}

	// 2. If kind:name (e.g. container:api-service or file:nginx-access)
	if strings.Contains(param, ":") {
		parts := strings.SplitN(param, ":", 2)
		err := s.configDB.RO.QueryRowContext(ctx,
			"SELECT id, kind, name FROM source WHERE kind = ? AND name = ?", parts[0], parts[1]).
			Scan(&meta.ID, &meta.Kind, &meta.Name)
		return meta, err
	}

	// 3. Name only
	err := s.configDB.RO.QueryRowContext(ctx,
		"SELECT id, kind, name FROM source WHERE name = ? OR kind || ':' || name = ? LIMIT 1", param, param).
		Scan(&meta.ID, &meta.Kind, &meta.Name)
	return meta, err
}

func parseLogTimeParam(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}

	// Try RFC 3339 Nano
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UnixNano(), nil
	}
	// Try RFC 3339
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UnixNano(), nil
	}
	// Try integer timestamp (seconds or nanoseconds)
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		// If less than 1e11 (year ~5138 in seconds), assume seconds
		if n < 100_000_000_000 {
			return n * 1_000_000_000, nil
		}
		return n, nil
	}

	return 0, errors.New("cannot parse time format")
}
