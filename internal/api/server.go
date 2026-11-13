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

	"github.com/dale-tomson/Tarsier/internal/auth"
	"github.com/dale-tomson/Tarsier/internal/config"
	"github.com/dale-tomson/Tarsier/internal/db"
)

// ServerConfig provides dependencies for the API and static server.
type ServerConfig struct {
	Config    *config.Config
	ConfigDB  *db.Handle
	MetricsDB *db.Handle
	Auth      *auth.Authenticator
	Sessions  *auth.SessionStore
	Limiter   *auth.RateLimiter
	Version   string
}

// SystemHealthResponse describes system operational state and memory metrics.
type SystemHealthResponse struct {
	Status        string              `json:"status"`
	Version       string              `json:"version"`
	GoVersion     string              `json:"go_version"`
	UptimeSeconds int64               `json:"uptime_seconds"`
	StartedAt     string              `json:"started_at"`
	Databases     map[string]DBHealth `json:"databases"`
	Memory        MemoryHealth        `json:"memory"`
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
	cfg          *config.Config
	configDB     *db.Handle
	metricsDB    *db.Handle
	auth         *auth.Authenticator
	sessions     *auth.SessionStore
	limiter      *auth.RateLimiter
	authHandlers *auth.Handlers
	assets       *AssetsHandler
	router       *chi.Mux
	mu           sync.RWMutex
	httpServer   *http.Server
	listener     net.Listener
	version      string
	startTime    time.Time
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

	s := &Server{
		cfg:          sc.Config,
		configDB:     sc.ConfigDB,
		metricsDB:    sc.MetricsDB,
		auth:         sc.Auth,
		sessions:     sc.Sessions,
		limiter:      sc.Limiter,
		authHandlers: authHandlers,
		assets:       assets,
		version:      version,
		startTime:    time.Now(),
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
