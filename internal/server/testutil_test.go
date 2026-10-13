package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ipedrazas/sbx/internal/db"
	"github.com/ipedrazas/sbx/internal/secretstore"
)

const testManageToken = "ds_testtoken"

// testEnv is a Server wired to an in-memory DB, an in-memory secret store and
// a temp data dir.
type testEnv struct {
	t       *testing.T
	srv     *Server
	handler http.Handler
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	secretstore.Init(secretstore.NewMemory())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pool, err := db.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	dataDir := t.TempDir()
	cfg := &DaemonConfig{ManageToken: testManageToken, AnthropicModel: defaultAnthropicModel}
	if err := cfg.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	srv := New(ctx, Options{DB: pool, DataDir: dataDir, Config: cfg, Port: 9820, DaemonURL: "http://127.0.0.1:9820"})
	return &testEnv{t: t, srv: srv, handler: srv.Handler()}
}

// do sends a request; body may be nil, a []byte/string (sent raw) or any value (JSON-encoded).
// token "" sends no Authorization header.
func (e *testEnv) do(method, path, token string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	var rdr io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rdr = bytes.NewReader(b)
	case string:
		rdr = bytes.NewReader([]byte(b))
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			e.t.Fatal(err)
		}
		rdr = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

// manage sends an authenticated management request.
func (e *testEnv) manage(method, path string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.do(method, path, testManageToken, body)
}

// decode parses a JSON response body into a generic map.
func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("invalid JSON (%d): %s", rec.Code, rec.Body.String())
	}
	return m
}
