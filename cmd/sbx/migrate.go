package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ipedrazas/sbx/internal/connectors/imap"
	"github.com/ipedrazas/sbx/internal/connectors/rest"
	"github.com/ipedrazas/sbx/internal/crud"
	"github.com/ipedrazas/sbx/internal/db"
	"github.com/ipedrazas/sbx/internal/models"
	"github.com/ipedrazas/sbx/internal/oauth"
	"github.com/ipedrazas/sbx/internal/secretstore"
	"github.com/ipedrazas/sbx/internal/server"
)

// secretKeysFor lists the secret-store keys a datasource owns.
func secretKeysFor(ds models.Datasource) []string {
	switch ds.DsType {
	case models.DatasourceGmail, models.DatasourceGoogleCalendar, models.DatasourceGoogleDrive,
		models.DatasourceJira, models.DatasourceLinear, models.DatasourceGithub, models.DatasourceHubspot:
		return []string{oauth.TokensKey(ds.ID)}
	case models.DatasourceRest:
		if cfg, err := rest.ParseDatasourceConfig(ds.Config); err == nil {
			if ref := cfg.Auth.SecretRef(); ref != nil {
				return []string{*ref}
			}
		}
	case models.DatasourceImap:
		if cfg, err := imap.ParseDatasourceConfig(ds.Config); err == nil {
			return []string{cfg.PasswordRef}
		}
	}
	return nil
}

// migrateSecrets copies secrets from the local store (Keychain/file) into
// OpenBao/Vault, including the manage token from config.toml.
func migrateSecrets(ctx context.Context, dataDir string) error {
	if os.Getenv("VAULT_ADDR") == "" || os.Getenv("VAULT_TOKEN") == "" {
		return errors.New("VAULT_ADDR and VAULT_TOKEN must be set for migrate-secrets")
	}

	// Step 1: read everything from the local (source) store.
	if err := initLocalSecretStore(dataDir, false); err != nil {
		return err
	}
	pool, err := db.Open(ctx, filepath.Join(dataDir, "dstudio.db"))
	if err != nil {
		return err
	}
	defer pool.Close()
	datasources, err := crud.ListDatasources(ctx, pool)
	if err != nil {
		return err
	}

	// The manage token lives in config.toml, not the secret store. Read the
	// file directly so VAULT_ADDR doesn't trigger a vault lookup.
	cfg, err := server.ReadConfig(dataDir)
	if err != nil {
		return fmt.Errorf("reading config.toml: %w", err)
	}

	keys := []string{"anthropic_api_key"}
	for _, ds := range datasources {
		keys = append(keys, secretKeysFor(ds)...)
	}

	type secret struct {
		key   string
		value []byte
	}
	var secrets []secret
	for _, k := range keys {
		v, err := secretstore.Get(k)
		switch {
		case err == nil:
			secrets = append(secrets, secret{k, v})
		case errors.Is(err, secretstore.ErrNotFound):
			fmt.Fprintf(os.Stderr, "  skip: %s (not found in local store)\n", k)
		default:
			fmt.Fprintf(os.Stderr, "  skip: %s (read error: %v)\n", k, err)
		}
	}
	if len(secrets) == 0 {
		fmt.Fprintln(os.Stderr, "No secrets to migrate.")
		return nil
	}

	total := len(secrets) + 1 // +1 for manage_token
	fmt.Fprintf(os.Stderr, "Found %d secrets to migrate into Vault at %s\n", total, os.Getenv("VAULT_ADDR"))

	// Step 2: write into Vault.
	vault := newVaultFromEnv()
	migrated, failed := 0, 0
	put := func(k string, v []byte) {
		if err := vault.Store(k, v); err != nil {
			fmt.Fprintf(os.Stderr, "  FAILED:   %s (%v)\n", k, err)
			failed++
			return
		}
		fmt.Fprintf(os.Stderr, "  migrated: %s\n", k)
		migrated++
	}
	put("manage_token", []byte(cfg.ManageToken))
	for _, s := range secrets {
		put(s.key, s.value)
	}

	fmt.Fprintf(os.Stderr, "\nDone: %d migrated, %d failed out of %d total.\n", migrated, failed, total)
	if failed > 0 {
		return fmt.Errorf("%d secrets failed to migrate", failed)
	}
	return nil
}
