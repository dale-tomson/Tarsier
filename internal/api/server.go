package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/dale-tomson/Tarsier/internal/alert"
	"github.com/dale-tomson/Tarsier/internal/auth"
	"github.com/dale-tomson/Tarsier/internal/cleanup"
	"github.com/dale-tomson/Tarsier/internal/config"
	"github.com/dale-tomson/Tarsier/internal/db"
	"github.com/dale-tomson/Tarsier/internal/logs"
	"github.com/dale-tomson/Tarsier/internal/metrics"
	"github.com/dale-tomson/Tarsier/internal/model"
)

// ServerConfig provides dependencies for the API and static server.
type ServerConfig struct {
	Config          *config.Config
	ConfigDB        *db.Handle
	MetricsDB       *db.Handle
	HotCache        *metrics.HotCache
	Rollup          *metrics.RollupEngine
	LogEngine       *logs.LogQueryEngine
	TailCache       *logs.TailCache
	Clock           model.Clock
	Auth            *auth.Authenticator
	Sessions        *auth.SessionStore
	Limiter         *auth.RateLimiter
	DockerConnected func() bool
	RuleStore       *alert.RuleStore
	RuleCache       *alert.RuleCache
	StateMachine    *alert.StateMachine
	Dispatcher      *alert.Dispatcher
	Heartbeat       *alert.HeartbeatWorker
	CleanupManager  *cleanup.CleanupManager
	ReportBuilder   *cleanup.ReportBuilder
	Deliverer       *cleanup.ReportDeliverer
	Maintainer      *cleanup.DatabaseMaintainer
	ConfigPath      string
	Version         string
}

type defaultServerClock struct{}

func (defaultServerClock) Now() time.Time {
	return time.Now().UTC()
}

// SystemHealthResponse describes system operational state and memory metrics.
type SystemHealthResponse struct {
	Status         string                 `json:"status"`
	Version        string                 `json:"version"`
	GoVersion      string                 `json:"go_version"`
	UptimeSeconds  int64                  `json:"uptime_seconds"`
	StartedAt      string                 `json:"started_at"`
	Databases      map[string]DBHealth    `json:"databases"`
	Memory         MemoryHealth           `json:"memory"`
	Heartbeat      *alert.HeartbeatStatus `json:"heartbeat,omitempty"`
	Storage        *StorageHealth         `json:"storage,omitempty"`
	CleanupWarning *CleanupWarningStatus  `json:"cleanup_warning,omitempty"`
}

// DBHealth describes SQLite database file status.
type DBHealth struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

// MemoryHealth captures Go runtime memory usage.
type MemoryHealth struct {
	AllocBytes      uint64 `json:"alloc_bytes"`
	TotalAllocBytes uint64 `json:"total_alloc_bytes"`
	SysBytes        uint64 `json:"sys_bytes"`
	NumGC           uint32 `json:"num_gc"`
}

// Server encapsulates the Chi HTTP router, static asset server, and background listeners.
type Server struct {
	cfg                 *config.Config
	configDB            *db.Handle
	metricsDB           *db.Handle
	hotCache            *metrics.HotCache
	rollup              *metrics.RollupEngine
	logEngine           *logs.LogQueryEngine
	tailCache           *logs.TailCache
	clock               model.Clock
	queryCache          *QueryCache
	auth                *auth.Authenticator
	sessions            *auth.SessionStore
	limiter             *auth.RateLimiter
	dockerConnectedFunc func() bool
	ruleStore           *alert.RuleStore
	ruleCache           *alert.RuleCache
	stateMachine        *alert.StateMachine
	dispatcher          *alert.Dispatcher
	heartbeat           *alert.HeartbeatWorker
	cleanupManager      *cleanup.CleanupManager
	reportBuilder       *cleanup.ReportBuilder
	deliverer           *cleanup.ReportDeliverer
	maintainer          *cleanup.DatabaseMaintainer
	configPath          string
	authHandlers        *auth.Handlers
	assets              *AssetsHandler
	router              *chi.Mux
	mu                  sync.RWMutex
	httpServer          *http.Server
	listener            net.Listener
	version             string
	startTime           time.Time
}

// NewServer builds and configures the HTTP router, middleware stack, and asset handlers.
func NewServer(sc ServerConfig) (*Server, error) {
	if sc.Config == nil {
		return nil, errors.New("config is required")
	}

	assets, err := NewAssetsHandler()
	if err != nil {
		return nil, fmt.Errorf("assets handler init: %w", err)
	}

	version := sc.Version
	if version == "" {
		version = "dev"
	}

	var authHandlers *auth.Handlers
	if sc.Auth != nil && sc.Sessions != nil && sc.Limiter != nil {
		authHandlers = auth.NewHandlers(sc.Auth, sc.Sessions, sc.Limiter, sc.Config.Server.BaseURL)
	}

	queryCache := NewQueryCache(256, 3*time.Second)

	clock := sc.Clock
	if clock == nil {
		clock = defaultServerClock{}
	}

	s := &Server{
		cfg:                 sc.Config,
		configDB:            sc.ConfigDB,
		metricsDB:           sc.MetricsDB,
		hotCache:            sc.HotCache,
		rollup:              sc.Rollup,
		logEngine:           sc.LogEngine,
		tailCache:           sc.TailCache,
		clock:               clock,
		queryCache:          queryCache,
		auth:                sc.Auth,
		sessions:            sc.Sessions,
		limiter:             sc.Limiter,
		dockerConnectedFunc: sc.DockerConnected,
		ruleStore:           sc.RuleStore,
		ruleCache:           sc.RuleCache,
		stateMachine:        sc.StateMachine,
		dispatcher:          sc.Dispatcher,
		heartbeat:           sc.Heartbeat,
		cleanupManager:      sc.CleanupManager,
		reportBuilder:       sc.ReportBuilder,
		deliverer:           sc.Deliverer,
		maintainer:          sc.Maintainer,
		configPath:          sc.ConfigPath,
		authHandlers:        authHandlers,
		assets:              assets,
		version:             version,
		startTime:           time.Now(),
	}

	s.setupRoutes()
	return s, nil
}

func (s *Server) setupRoutes() {
	r := chi.NewRouter()

	// Global Middlewares
	r.Use(middleware.Recoverer)
	r.Use(middleware.RealIP)
	r.Use(middleware.Compress(5))
	r.Use(maxBytesMiddleware(1 << 20)) // 1 MB request body limit

	// Unauthenticated health probe
	r.Get("/healthz", s.handleHealthz)

	// API Routes (/api/v1)
	r.Route("/api/v1", func(api chi.Router) {
		if s.authHandlers != nil {
			// Login is public but bounded by IP rate limiting
			api.Post("/auth/login", s.authHandlers.LoginHandler)

			// Authenticated routes
			api.Group(func(authGroup chi.Router) {
				authGroup.Use(s.authHandlers.RequireAuth)

				authGroup.Post("/auth/logout", s.authHandlers.LogoutHandler)
				authGroup.Get("/auth/me", s.authHandlers.MeHandler)

				authGroup.Get("/system/config", s.handleSystemConfig)
				authGroup.Get("/system/health", s.handleSystemHealth)

				authGroup.Get("/host/metrics", s.handleHostMetrics)
				authGroup.Get("/metrics/daily/summary", s.handleDailySummary)
				authGroup.Get("/metrics/daily", s.handleDailyHistory)

				authGroup.Get("/containers", s.handleContainersList)
				authGroup.Get("/containers/{name}", s.handleContainerDetail)

				authGroup.Get("/sources", s.handleSourcesList)
				authGroup.Get("/logs", s.handleLogsQuery)
				authGroup.Get("/logs/context", s.handleLogsContext)

				// Alerts & Rules
				authGroup.Get("/alerts/active", s.handleActiveAlerts)
				authGroup.Get("/alerts/rules", s.handleListRules)
				authGroup.Post("/alerts/rules", s.handleCreateRule)
				authGroup.Get("/alerts/rules/{id}", s.handleGetRule)
				authGroup.Put("/alerts/rules/{id}", s.handleUpdateRule)
				authGroup.Delete("/alerts/rules/{id}", s.handleDeleteRule)
				authGroup.Post("/alerts/rules/{id}/enable", s.handleEnableRule)
				authGroup.Post("/alerts/rules/{id}/disable", s.handleDisableRule)
				authGroup.Get("/alerts/templates", s.handleGetTemplates)
				authGroup.Post("/alerts/rules/preview", s.handlePreviewRule)
				authGroup.Get("/alerts/rules/export", s.handleExportRules)
				authGroup.Post("/alerts/rules/import", s.handleImportRules)
				authGroup.Get("/alerts/history", s.handleAlertHistory)
				authGroup.Post("/alerts/test", s.handleTestNotification)

				// Cleanup & Retention
				authGroup.Get("/system/cleanup/history", s.handleCleanupHistory)
				authGroup.Get("/system/cleanup/{id}/report", s.handleCleanupReport)
				authGroup.Post("/system/cleanup/run", s.handleCleanupRun)
				authGroup.Get("/system/cleanup/template", s.handleCleanupTemplateDownload)
				authGroup.Post("/system/cleanup/preview", s.handleCleanupPreview)
				authGroup.Post("/system/reload", s.handleSystemReload)
			})
		}
	})

	// Static Assets
	r.Get("/static/*", s.assets.ServeStatic)

	// SPA Shell & Fallback for non-API client routes
	r.Get("/", s.assets.ServeSPA)
	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasPrefix(req.URL.Path, "/api/") || strings.HasPrefix(req.URL.Path, "/static/") {
			http.NotFound(w, req)
			return
		}
		s.assets.ServeSPA(w, req)
	})

	s.router = r
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	auth.WriteJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"version": s.version,
	})
}

func (s *Server) handleSystemConfig(w http.ResponseWriter, r *http.Request) {
	sanitized := s.cfg.Sanitized()
	auth.WriteJSON(w, http.StatusOK, sanitized)
}

func (s *Server) handleSystemHealth(w http.ResponseWriter, r *http.Request) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	cfgPath := ""
	if s.configDB != nil {
		cfgPath = s.configDB.Path
	}
	metricsPath := ""
	if s.metricsDB != nil {
		metricsPath = s.metricsDB.Path
	}

	var hbStatus *alert.HeartbeatStatus
	if s.heartbeat != nil {
		st := s.heartbeat.Status()
		hbStatus = &st
	}

	storage, warning := s.inspectStorage(r.Context())

	resp := SystemHealthResponse{
		Status:        "ok",
		Version:       s.version,
		GoVersion:     runtime.Version(),
		UptimeSeconds: int64(time.Since(s.startTime).Seconds()),
		StartedAt:     s.startTime.UTC().Format(time.RFC3339),
		Databases: map[string]DBHealth{
			"config":  {Path: cfgPath, Status: "ok"},
			"metrics": {Path: metricsPath, Status: "ok"},
		},
		Memory: MemoryHealth{
			AllocBytes:      m.Alloc,
			TotalAllocBytes: m.TotalAlloc,
			SysBytes:        m.Sys,
			NumGC:           m.NumGC,
		},
		Heartbeat:      hbStatus,
		Storage:        storage,
		CleanupWarning: warning,
	}

	auth.WriteJSON(w, http.StatusOK, resp)
}

// Router returns the configured chi.Mux for testing or custom wrapping.
func (s *Server) Router() *chi.Mux {
	return s.router
}

// Start begins listening on the configured address.
func (s *Server) Start() error {
	addr := s.cfg.Server.Listen
	if addr == "" {
		addr = ":8080"
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}

	return s.StartListener(ln)
}

// StartListener begins serving on an already created net.Listener (useful for ephemeral ports in tests).
func (s *Server) StartListener(ln net.Listener) error {
	s.mu.Lock()
	s.listener = ln
	s.httpServer = &http.Server{
		Handler:           s.router,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	srv := s.httpServer
	s.mu.Unlock()

	return srv.Serve(ln)
}

// Addr returns the bound listener address if listening, or empty string.
func (s *Server) Addr() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.listener != nil {
		return s.listener.Addr().String()
	}
	return ""
}

// Shutdown gracefully stops the HTTP server.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.RLock()
	srv := s.httpServer
	s.mu.RUnlock()
	if srv != nil {
		return srv.Shutdown(ctx)
	}
	return nil
}

func maxBytesMiddleware(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, limit)
			}
			next.ServeHTTP(w, r)
		})
	}
}
