package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ipedrazas/sbx/internal/crud"
	"github.com/ipedrazas/sbx/internal/db"
	"github.com/ipedrazas/sbx/internal/models"
	"github.com/ipedrazas/sbx/internal/oauth"
	"github.com/ipedrazas/sbx/internal/secretstore"
	"github.com/ipedrazas/sbx/internal/server"
)

func TestParseArgs(t *testing.T) {
	c, err := parseArgs(nil)
	if err != nil || c.host != "127.0.0.1" || c.port != 9820 || c.dataDir != "~/.dstudio" || c.command != "" {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	c, err = parseArgs([]string{"--host", "0.0.0.0", "--port", "1234", "migrate-secrets", "--data-dir", "/tmp/x"})
	if err != nil || c.host != "0.0.0.0" || c.port != 1234 || c.command != "migrate-secrets" || c.dataDir != "/tmp/x" {
		t.Fatalf("flags around subcommand: %+v %v", c, err)
	}
	if c, _ := parseArgs([]string{"version"}); c.command != "version" {
		t.Fatal("version subcommand")
	}
	for _, bad := range [][]string{{"bogus"}, {"version", "extra"}, {"--port", "70000"}, {"--nope"}} {
		if _, err := parseArgs(bad); err == nil {
			t.Errorf("%v should fail", bad)
		}
	}
}

func TestExpandPath(t *testing.T) {
	home, _ := os.UserHomeDir()
	if got := expandPath("~/.dstudio"); got != filepath.Join(home, ".dstudio") {
		t.Fatalf("got %s", got)
	}
	if got := expandPath("/abs"); got != "/abs" {
		t.Fatalf("got %s", got)
	}
}

func TestRunVersionAndOpenAPI(t *testing.T) {
	if err := run([]string{"version"}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"--dump-openapi"}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"-h"}); err != nil {
		t.Fatal(err)
	}
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// TestRunServesAndShutsDown boots the daemon end to end, probes health, then
// sends SIGTERM to trigger a graceful shutdown.
func TestRunServesAndShutsDown(t *testing.T) {
	t.Setenv("DSTUDIO_SECRET_STORE", "file")
	t.Setenv("VAULT_ADDR", "")
	t.Setenv("DSTUDIO_MANAGE_TOKEN", "tok")
	dir := t.TempDir()
	port := freePort(t)
	done := make(chan error, 1)
	go func() { done <- run([]string{"--data-dir", dir, "--port", strconv.Itoa(port)}) }()

	url := "http://127.0.0.1:" + strconv.Itoa(port)
	var resp *http.Response
	var err error
	for i := 0; i < 100; i++ {
		resp, err = http.Get(url + "/manage/health")
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("daemon never came up: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("health: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("GET", url+"/manage/stats", nil)
	req.Header.Set("Authorization", "Bearer tok")
	if r, err := http.DefaultClient.Do(req); err != nil || r.StatusCode != 200 {
		t.Fatalf("stats: %v %v", r, err)
	}

	_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("daemon did not shut down")
	}
	if _, err := os.Stat(filepath.Join(dir, "dstudio.db")); err != nil {
		t.Fatal("database not created")
	}
}

func TestMigrateSecrets(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	t.Setenv("DSTUDIO_SECRET_STORE", "file")

	t.Setenv("VAULT_ADDR", "")
	if err := migrateSecrets(ctx, dir); err == nil {
		t.Fatal("expected error without VAULT_ADDR")
	}

	// Seed the local file store and DB.
	fs, _ := secretstore.NewFile(filepath.Join(dir, "secrets"))
	secretstore.Init(fs)
	pool, err := db.Open(ctx, filepath.Join(dir, "dstudio.db"))
	if err != nil {
		t.Fatal(err)
	}
	gid, _ := crud.CreateDatasource(ctx, pool, "g", "gmail", `{"client_id":"c"}`, nil)
	_ = oauth.StoreTokens(gid, &oauth.Tokens{AccessToken: "at"})
	_, _ = crud.CreateDatasource(ctx, pool, "r", "rest", `{"base_url":"http://x","auth":{"kind":"bearer","token_ref":"rest-ref"}}`, nil)
	_ = secretstore.Put("rest-ref", []byte("bearer-secret"))
	_, _ = crud.CreateDatasource(ctx, pool, "i", "imap", `{"imap_host":"imap.x","username":"u","password_ref":"imap-ref"}`, nil)
	pool.Close()
	cfg := &server.DaemonConfig{ManageToken: "mt", AnthropicModel: "m"}
	if err := cfg.Save(dir); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	stored := map[string]string{}
	vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Data struct {
				Value string `json:"value"`
			} `json:"data"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		v, _ := base64.StdEncoding.DecodeString(body.Data.Value)
		mu.Lock()
		stored[r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]] = string(v)
		mu.Unlock()
	}))
	defer vault.Close()
	t.Setenv("VAULT_ADDR", vault.URL)
	t.Setenv("VAULT_TOKEN", "t")

	if err := migrateSecrets(ctx, dir); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"manage_token": "mt", oauth.TokensKey(gid): `{"access_token":"at","refresh_token":null,"expires_at":null}`, "rest-ref": "bearer-secret"}
	for k, v := range want {
		if stored[k] != v {
			t.Errorf("%s: got %q want %q", k, stored[k], v)
		}
	}
	if _, ok := stored["imap-ref"]; ok {
		t.Error("missing imap secret should be skipped")
	}
}

func TestSecretKeysFor(t *testing.T) {
	if got := secretKeysFor(models.Datasource{ID: 3, DsType: models.DatasourceLinear}); len(got) != 1 || got[0] != "oauth_tokens_3" {
		t.Fatalf("got %v", got)
	}
	if got := secretKeysFor(models.Datasource{DsType: models.DatasourcePostgres}); got != nil {
		t.Fatalf("got %v", got)
	}
}

func TestInitSecretStoreVault(t *testing.T) {
	t.Setenv("VAULT_ADDR", "http://vault")
	t.Setenv("VAULT_TOKEN", "t")
	t.Setenv("VAULT_MOUNT", "kv")
	t.Setenv("VAULT_PREFIX", "p")
	if err := initSecretStore(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	v := newVaultFromEnv()
	if v.Mount != "kv" || v.Prefix != "p" {
		t.Fatalf("got %+v", v)
	}
}

func TestLogLevel(t *testing.T) {
	t.Setenv("DSTUDIO_LOG", "")
	t.Setenv("RUST_LOG", "debug,sqlx=warn")
	if logLevel() != slog.LevelDebug {
		t.Fatal("RUST_LOG fallback")
	}
	t.Setenv("DSTUDIO_LOG", "error")
	if logLevel() != slog.LevelError {
		t.Fatal("DSTUDIO_LOG should win")
	}
	t.Setenv("DSTUDIO_LOG", "warn")
	if logLevel() != slog.LevelWarn {
		t.Fatal("warn")
	}
	t.Setenv("DSTUDIO_LOG", "bogus")
	if logLevel() != slog.LevelInfo {
		t.Fatal("unknown → info")
	}
}
