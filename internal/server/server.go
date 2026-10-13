// Package server is the daemon's HTTP surface: the bearer-gated management
// API under /manage, the agent-facing live dataset API under /a, the actions
// API under /act, and the OAuth callback.
package server

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ipedrazas/sbx/internal/audit"
	"github.com/ipedrazas/sbx/internal/crud"
	"github.com/ipedrazas/sbx/internal/oauth"
	"github.com/ipedrazas/sbx/internal/version"
)

// AIRuntime is the mutable AI-assist config (the key lives in the secret store).
type AIRuntime struct {
	Enabled bool
	Model   string
}

// Options configures a Server.
type Options struct {
	DB      *sql.DB
	DataDir string
	Config  *DaemonConfig
	Port    int
	// DaemonURL is the externally reachable URL written into agent configs.
	DaemonURL string
	// OAuthRedirectURL overrides http://127.0.0.1:{port}/callback when non-empty.
	OAuthRedirectURL string
	Audit            audit.Sender
}

// Server holds the shared daemon state (Rust `AppState`).
type Server struct {
	db               *sql.DB
	dataDir          string
	manageToken      string
	daemonPort       int
	daemonURL        string
	oauthRedirectURL string
	audit            audit.Sender
	metrics          *Metrics
	rateLimiter      *RateLimiter
	startedAt        time.Time

	// ctx scopes background work (syncs, snapshots) spawned by handlers.
	ctx context.Context

	syncMu      sync.Mutex
	syncCancels map[int64]context.CancelFunc

	oauthMu      sync.Mutex
	pendingOAuth map[string]oauth.PendingFlow

	aiMu sync.RWMutex
	ai   AIRuntime
}

// New builds a Server. ctx bounds background goroutines started by handlers.
func New(ctx context.Context, o Options) *Server {
	cfg := o.Config
	if cfg == nil {
		cfg = &DaemonConfig{AnthropicModel: defaultAnthropicModel}
	}
	return &Server{
		db:               o.DB,
		dataDir:          o.DataDir,
		manageToken:      cfg.ManageToken,
		daemonPort:       o.Port,
		daemonURL:        o.DaemonURL,
		oauthRedirectURL: o.OAuthRedirectURL,
		audit:            o.Audit,
		metrics:          NewMetrics(),
		rateLimiter:      NewRateLimiter(),
		startedAt:        time.Now(),
		ctx:              ctx,
		syncCancels:      map[int64]context.CancelFunc{},
		pendingOAuth:     map[string]oauth.PendingFlow{},
		ai:               AIRuntime{Enabled: cfg.AIEnabled, Model: cfg.AnthropicModel},
	}
}

func (s *Server) aiRuntime() AIRuntime {
	s.aiMu.RLock()
	defer s.aiMu.RUnlock()
	return s.ai
}

// Handler returns the daemon's root HTTP handler.
func (s *Server) Handler() http.Handler {
	manage := http.NewServeMux()
	manage.HandleFunc("GET /manage/stats", s.handleStats)
	s.registerDatasources(manage)
	s.registerDatasets(manage)
	s.registerAgents(manage)
	s.registerActions(manage)
	s.registerApprovals(manage)
	s.registerAudit(manage)
	s.registerSettings(manage)
	s.registerOAuth(manage)

	live := http.NewServeMux()
	s.registerLive(live)

	act := http.NewServeMux()
	s.registerAct(act)

	root := http.NewServeMux()
	root.HandleFunc("GET /manage/health", s.handleHealth)
	root.Handle("/manage/", s.requireManageAuth(manage))
	root.HandleFunc("GET /callback", s.handleOAuthCallback)
	root.Handle("/a/", s.recordAgentMetrics(live))
	root.Handle("/act/", act)
	registerOpenAPI(root)
	return logRequests(root)
}

// handle registers h for each "METHOD path" pattern; patterns ending in "/"
// for a collection root are also registered without the trailing slash.
func handle(mux *http.ServeMux, h http.HandlerFunc, patterns ...string) {
	for _, p := range patterns {
		mux.HandleFunc(p, h)
	}
}

func (s *Server) requireManageAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, _ := bearerToken(r)
		if tok == "" || tok != s.manageToken {
			writeErrorOnly(w, http.StatusUnauthorized, "invalid or missing management token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// statusRecorder captures the status code and bytes written.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  uint64
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += uint64(n)
	return n, err
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func elapsedMS(start time.Time) uint32 {
	ms := time.Since(start).Milliseconds()
	if ms > int64(^uint32(0)) {
		return ^uint32(0)
	}
	return uint32(ms)
}

// recordAgentMetrics enforces per-binding rate limits, records a metrics
// sample, and emits an audit event for every /a/* request.
func (s *Server) recordAgentMetrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/a")
		agentID, alias := extractAgentAlias(path)
		endpoint := extractEndpoint(path)
		start := time.Now()

		var datasetID *int64
		var decision *RateDecision
		if agentID != nil && alias != nil {
			b, err := crud.GetAgentBinding(r.Context(), s.db, *agentID, *alias)
			if err == nil && b != nil {
				id := b.DatasetID
				datasetID = &id
				if b.RateLimitCount != nil && b.RateLimitPeriodSeconds != nil &&
					*b.RateLimitCount > 0 && *b.RateLimitPeriodSeconds > 0 {
					d := s.rateLimiter.Check(*agentID, BindingDataset, b.DatasetID,
						uint32(*b.RateLimitCount), uint32(*b.RateLimitPeriodSeconds))
					decision = &d
				}
			}
		}

		if decision != nil && !decision.Allowed {
			retry := strconv.FormatUint(uint64(decision.RetryAfter), 10)
			w.Header().Set("Retry-After", retry)
			w.Header().Set("X-RateLimit-Limit", strconv.FormatUint(uint64(decision.Limit), 10))
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", retry)
			rec := &statusRecorder{ResponseWriter: w}
			writeJSON(rec, http.StatusTooManyRequests, map[string]any{"error": "rate limit exceeded", "retry_after": decision.RetryAfter})
			s.finishAgentRequest(agentID, datasetID, alias, endpoint, http.StatusTooManyRequests, rec.bytes, start)
			return
		}
		if decision != nil {
			w.Header().Set("X-RateLimit-Limit", strconv.FormatUint(uint64(decision.Limit), 10))
			w.Header().Set("X-RateLimit-Remaining", strconv.FormatUint(uint64(decision.Remaining), 10))
			w.Header().Set("X-RateLimit-Reset", strconv.FormatUint(uint64(decision.ResetSeconds), 10))
		}

		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		s.finishAgentRequest(agentID, datasetID, alias, endpoint, rec.status, rec.bytes, start)
	})
}

func (s *Server) finishAgentRequest(agentID, datasetID *int64, alias *string, endpoint string, status int, bytesOut uint64, start time.Time) {
	latency := elapsedMS(start)
	if datasetID != nil {
		s.metrics.Record(*datasetID, RequestSample{
			Timestamp: time.Now().Unix(),
			LatencyMS: latency,
			Status:    uint16(status),
			BytesOut:  bytesOut,
		})
	}
	s.audit.Send(audit.AuditEvent{
		AgentID:   agentID,
		DatasetID: datasetID,
		Alias:     alias,
		Endpoint:  endpoint,
		Status:    uint16(status),
		BytesOut:  bytesOut,
		LatencyMs: latency,
	})
}

// extractAgentAlias parses `/{agent_id}/{alias}/...` (prefix /a stripped).
// The discovery endpoint `/{agent_id}/datasets` has no alias.
func extractAgentAlias(path string) (*int64, *string) {
	segs := strings.Split(strings.TrimPrefix(path, "/"), "/")
	var agentID *int64
	if v, err := strconv.ParseInt(segs[0], 10, 64); err == nil {
		agentID = &v
	}
	var alias *string
	if len(segs) > 1 && segs[1] != "datasets" {
		a := segs[1]
		alias = &a
	}
	return agentID, alias
}

// extractEndpoint classifies the request by its operation segment.
func extractEndpoint(path string) string {
	segs := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(segs) == 2 && segs[1] == "datasets" {
		return "datasets"
	}
	if len(segs) >= 3 {
		return segs[2]
	}
	return "unknown"
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	body := map[string]any{
		"version":    version.Version,
		"git_sha":    version.GitSHA,
		"build_time": version.BuildTime,
	}
	var one int
	if err := s.db.QueryRowContext(r.Context(), "SELECT 1").Scan(&one); err != nil {
		body["status"] = "degraded"
		body["reason"] = "database unreachable"
		writeJSON(w, http.StatusServiceUnavailable, body)
		return
	}
	body["status"] = "ok"
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	count := func(table string) int64 {
		var n int64
		if err := s.db.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
			return 0
		}
		return n
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"uptime_seconds": int64(time.Since(s.startedAt).Seconds()),
		"datasources":    count("datasources"),
		"datasets":       count("datasets"),
		"agents":         count("agents"),
	})
}

// logRequests logs each request at debug level (tower-http TraceLayer).
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		slog.Debug("request", "method", r.Method, "path", r.URL.Path, "status", rec.status, "latency", time.Since(start))
	})
}
