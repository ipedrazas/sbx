package server

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"

	"github.com/ipedrazas/sbx/internal/secretstore"
	"github.com/ipedrazas/sbx/internal/util"
)

const configFilename = "config.toml"

// DaemonConfig is persisted at <data_dir>/config.toml.
type DaemonConfig struct {
	// Shared bearer token required on all /manage/* requests.
	ManageToken string `toml:"manage_token"`
	// Days to keep audit_log rows; nil keeps them forever.
	AuditRetentionDays *uint32 `toml:"audit_retention_days,omitempty"`
	// Toggles AI assist without deleting the stored key.
	AIEnabled bool `toml:"ai_enabled"`
	// Model used for AI assist.
	AnthropicModel string `toml:"anthropic_model"`
}

const defaultAnthropicModel = "claude-sonnet-4-6"

func defaultRetention() *uint32 { v := uint32(30); return &v }

// ConfigPath returns <dataDir>/config.toml.
func ConfigPath(dataDir string) string { return filepath.Join(dataDir, configFilename) }

// LoadOrInitConfig loads config.toml, creating it with a fresh manage token
// when missing or unreadable. The token is then synced with Vault (when
// VAULT_ADDR is set) and overridden by DSTUDIO_MANAGE_TOKEN when non-empty.
func LoadOrInitConfig(dataDir string) (*DaemonConfig, error) {
	path := ConfigPath(dataDir)
	var cfg *DaemonConfig
	if _, err := os.Stat(path); err == nil {
		c, err := ReadConfig(dataDir)
		if err != nil {
			slog.Warn("daemon config is unreadable; regenerating", "path", path)
			if cfg, err = generateConfig(dataDir); err != nil {
				return nil, err
			}
		} else {
			cfg = c
		}
	} else {
		var err error
		if cfg, err = generateConfig(dataDir); err != nil {
			return nil, err
		}
	}

	syncManageTokenWithVault(&cfg.ManageToken)

	if tok := os.Getenv("DSTUDIO_MANAGE_TOKEN"); tok != "" {
		slog.Info("using manage_token from DSTUDIO_MANAGE_TOKEN env var")
		cfg.ManageToken = tok
	}
	return cfg, nil
}

// ReadConfig parses config.toml, applying serde defaults for missing keys.
func ReadConfig(dataDir string) (*DaemonConfig, error) {
	raw, err := os.ReadFile(ConfigPath(dataDir))
	if err != nil {
		return nil, err
	}
	cfg := DaemonConfig{AuditRetentionDays: defaultRetention(), AnthropicModel: defaultAnthropicModel}
	md, err := toml.Decode(string(raw), &cfg)
	if err != nil {
		return nil, err
	}
	if !md.IsDefined("manage_token") {
		return nil, errors.New("missing field `manage_token`")
	}
	return &cfg, nil
}

func generateConfig(dataDir string) (*DaemonConfig, error) {
	cfg := &DaemonConfig{
		ManageToken:        util.GenerateBearerToken(),
		AuditRetentionDays: defaultRetention(),
		AnthropicModel:     defaultAnthropicModel,
	}
	if err := cfg.Save(dataDir); err != nil {
		return nil, err
	}
	slog.Info("generated new daemon management token", "path", ConfigPath(dataDir))
	return cfg, nil
}

// Save writes the config with 0600 permissions.
func (c *DaemonConfig) Save(dataDir string) error {
	path := ConfigPath(dataDir)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := toml.NewEncoder(f).Encode(c); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

const manageTokenKey = "manage_token"

func syncManageTokenWithVault(token *string) {
	if _, ok := os.LookupEnv("VAULT_ADDR"); !ok {
		return
	}
	b, err := secretstore.Get(manageTokenKey)
	switch {
	case err == nil:
		if len(b) > 0 {
			slog.Info("loaded manage_token from vault")
			*token = string(b)
		}
	case errors.Is(err, secretstore.ErrNotFound):
		slog.Info("seeding manage_token into vault")
		if err := secretstore.Put(manageTokenKey, []byte(*token)); err != nil {
			slog.Warn("failed to seed manage_token into vault", "err", err)
		}
	default:
		slog.Warn("vault manage_token lookup failed, using local", "err", err)
	}
}
