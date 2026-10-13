// Command sbx is the data gateway daemon: it serves the management API and
// the live dataset/action APIs that agents consume.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ipedrazas/sbx/internal/actions"
	"github.com/ipedrazas/sbx/internal/agentdir"
	"github.com/ipedrazas/sbx/internal/audit"
	"github.com/ipedrazas/sbx/internal/crud"
	"github.com/ipedrazas/sbx/internal/db"
	"github.com/ipedrazas/sbx/internal/secretstore"
	"github.com/ipedrazas/sbx/internal/server"
	"github.com/ipedrazas/sbx/internal/version"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

type cli struct {
	host        string
	port        int
	dataDir     string
	dumpOpenAPI bool
	showVersion bool
	command     string
}

func parseArgs(args []string) (*cli, error) {
	c := &cli{}
	fs := flag.NewFlagSet("sbx", flag.ContinueOnError)
	fs.StringVar(&c.host, "host", "127.0.0.1", "Address to listen on")
	fs.IntVar(&c.port, "port", 9820, "Port for the API server")
	fs.StringVar(&c.dataDir, "data-dir", "~/.dstudio", "Path to the data directory (config DB and cache DBs)")
	fs.BoolVar(&c.dumpOpenAPI, "dump-openapi", false, "Print the OpenAPI spec as JSON and exit")
	fs.BoolVar(&c.showVersion, "version", false, "Print version and exit")
	fs.BoolVar(&c.showVersion, "V", false, "Print version and exit")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "sbx — data gateway daemon for AI agents\n\nUsage: sbx [OPTIONS] [COMMAND]\n\nCommands:\n"+
			"  migrate-secrets  Migrate secrets from the local store (Keychain/file) into OpenBao/Vault\n"+
			"  version          Print version and build provenance, then exit\n\nOptions:\n")
		fs.PrintDefaults()
	}
	// Allow flags before and after the subcommand, like clap.
	var rest []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) > 0 {
			rest = append(rest, args[0])
			args = args[1:]
		}
	}
	if len(rest) > 1 {
		return nil, fmt.Errorf("unexpected argument '%s'", rest[1])
	}
	if len(rest) == 1 {
		switch rest[0] {
		case "migrate-secrets", "version":
			c.command = rest[0]
		default:
			return nil, fmt.Errorf("unrecognized subcommand '%s'", rest[0])
		}
	}
	if c.port < 0 || c.port > 65535 {
		return nil, fmt.Errorf("invalid port %d", c.port)
	}
	return c, nil
}

// logLevel reads DSTUDIO_LOG (or RUST_LOG for compatibility); the first one set wins.
func logLevel() slog.Level {
	for _, key := range []string{"DSTUDIO_LOG", "RUST_LOG"} {
		v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
		if v == "" {
			continue
		}
		// RUST_LOG may be a directive list like "info,sqlx=warn"; use the first term.
		v, _, _ = strings.Cut(v, ",")
		switch v {
		case "debug", "trace":
			return slog.LevelDebug
		case "warn":
			return slog.LevelWarn
		case "error":
			return slog.LevelError
		}
		return slog.LevelInfo
	}
	return slog.LevelInfo
}

func setupLogging() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel()})))
}

func run(args []string) error {
	setupLogging()
	c, err := parseArgs(args)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}

	if c.showVersion || c.command == "version" {
		fmt.Println("sbx " + version.Long())
		return nil
	}
	if c.dumpOpenAPI {
		b, err := server.OpenAPISpec()
		if err != nil {
			return err
		}
		fmt.Println(string(b))
		return nil
	}

	slog.Info("starting sbx", "version", version.Version, "git_sha", version.GitSHA, "build_time", version.BuildTime)

	dataDir := expandPath(c.dataDir)
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if c.command == "migrate-secrets" {
		return migrateSecrets(ctx, dataDir)
	}

	if err := initSecretStore(dataDir); err != nil {
		return err
	}

	dbPath := filepath.Join(dataDir, "dstudio.db")
	slog.Info("opening database", "path", dbPath)
	pool, err := db.Open(ctx, dbPath)
	if err != nil {
		return err
	}
	defer pool.Close()

	cfg, err := server.LoadOrInitConfig(dataDir)
	if err != nil {
		return err
	}

	auditTx := audit.SpawnWriter(ctx, pool)
	audit.SpawnPruner(ctx, pool, cfg.AuditRetentionDays)
	actions.SpawnExpirySweeper(ctx, pool)

	go func() {
		if n, err := actions.BackfillPredefinedActions(ctx, pool); err != nil {
			slog.Warn("predefined actions backfill failed", "err", err)
		} else if n > 0 {
			slog.Info("predefined actions backfill", "seeded", n)
		}
		if n, err := actions.SyncPredefinedActionSchemas(ctx, pool); err != nil {
			slog.Warn("predefined action schema sync failed", "err", err)
		} else if n > 0 {
			slog.Info("predefined action schema sync", "updated", n)
		}
	}()

	go func() {
		if n := server.MigrateLinearAPIKeys(ctx, pool); n > 0 {
			slog.Info("Linear api_key migration: relocated keys to the secret store", "count", n)
		}
	}()

	// 0.0.0.0 can't be used in a URL — fall back to loopback.
	urlHost := c.host
	if urlHost == "0.0.0.0" {
		urlHost = "127.0.0.1"
	}
	daemonURL := "http://" + net.JoinHostPort(urlHost, strconv.Itoa(c.port))

	go func() {
		agents, err := crud.ListAgents(ctx, pool)
		if err != nil {
			slog.Warn("startup agent sync: list_agents failed", "err", err)
			return
		}
		for _, a := range agents {
			if _, err := agentdir.SyncAgentDir(ctx, pool, dataDir, a.ID, nil, daemonURL); err != nil {
				slog.Warn("startup agent sync failed", "agent_id", a.ID, "agent", a.Name, "err", err)
			}
		}
		slog.Info("startup agent sync complete")
	}()

	redirect := os.Getenv("DSTUDIO_OAUTH_REDIRECT_URL")
	if redirect != "" {
		slog.Info("OAuth redirect URL override", "url", redirect)
	}

	srv := server.New(ctx, server.Options{
		DB:               pool,
		DataDir:          dataDir,
		Config:           cfg,
		Port:             c.port,
		DaemonURL:        daemonURL,
		OAuthRedirectURL: redirect,
		Audit:            auditTx,
	})

	addr := net.JoinHostPort(c.host, strconv.Itoa(c.port))
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		slog.Info("sbx listening", "addr", addr)
		errCh <- httpSrv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		slog.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	}
}

func expandPath(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return p
}

func newVaultFromEnv() *secretstore.Vault {
	v := secretstore.NewVault(os.Getenv("VAULT_ADDR"), os.Getenv("VAULT_TOKEN"))
	if m, ok := os.LookupEnv("VAULT_MOUNT"); ok {
		v.Mount = m
	}
	if p, ok := os.LookupEnv("VAULT_PREFIX"); ok {
		v.Prefix = p
	}
	return v
}

// initSecretStore picks Vault when configured, else the macOS Keychain, else
// files under <data_dir>/secrets.
func initSecretStore(dataDir string) error {
	if os.Getenv("VAULT_ADDR") != "" && os.Getenv("VAULT_TOKEN") != "" {
		secretstore.Init(newVaultFromEnv())
		slog.Info("using OpenBao/Vault for secret storage", "addr", os.Getenv("VAULT_ADDR"))
		return nil
	}
	return initLocalSecretStore(dataDir, true)
}

func initLocalSecretStore(dataDir string, logChoice bool) error {
	if os.Getenv("DSTUDIO_SECRET_STORE") != "file" && secretstore.KeychainAvailable() {
		secretstore.Init(secretstore.Keychain{})
		if logChoice {
			slog.Info("using macOS Keychain for secret storage")
		}
		return nil
	}
	fs, err := secretstore.NewFile(filepath.Join(dataDir, "secrets"))
	if err != nil {
		return err
	}
	secretstore.Init(fs)
	if logChoice {
		slog.Info("using file-based secret storage")
	}
	return nil
}
