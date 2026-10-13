// Package factory builds the right connector for a datasource (and dataset).
package factory

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"

	"github.com/ipedrazas/sbx/internal/connector"
	"github.com/ipedrazas/sbx/internal/connectors/fileupload"
	"github.com/ipedrazas/sbx/internal/connectors/gcal"
	"github.com/ipedrazas/sbx/internal/connectors/gdrive"
	"github.com/ipedrazas/sbx/internal/connectors/github"
	"github.com/ipedrazas/sbx/internal/connectors/gmail"
	"github.com/ipedrazas/sbx/internal/connectors/imap"
	"github.com/ipedrazas/sbx/internal/connectors/jira"
	"github.com/ipedrazas/sbx/internal/connectors/linear"
	"github.com/ipedrazas/sbx/internal/connectors/postgres"
	"github.com/ipedrazas/sbx/internal/connectors/rest"
	"github.com/ipedrazas/sbx/internal/crud"
	"github.com/ipedrazas/sbx/internal/models"
	"github.com/ipedrazas/sbx/internal/oauth"
	"github.com/ipedrazas/sbx/internal/secretstore"
)

// Close releases resources held by a connector (the Postgres pool). Rust
// freed these on drop; Go callers must call Close when done.
func Close(c connector.Connector) {
	if cl, ok := c.(interface{ Close() }); ok {
		cl.Close()
	}
}

func loadSecret(key string) (string, error) {
	b, err := secretstore.Get(key)
	if err != nil {
		return "", connector.Auth("%s", err.Error())
	}
	return string(b), nil
}

// CreateConnector builds a connector for a datasource without dataset config
// (test_connection, list_tables, ...).
func CreateConnector(ctx context.Context, db *sql.DB, ds *models.Datasource) (connector.Connector, error) {
	return create(ctx, db, ds, map[string]any{})
}

// CreateConnectorForDataset merges the datasource config with the dataset's source_config.
func CreateConnectorForDataset(ctx context.Context, db *sql.DB, ds *models.Datasource, dataset *models.Dataset) (connector.Connector, error) {
	sc := dataset.SourceConfig
	if sc == nil {
		sc = map[string]any{}
	}
	return create(ctx, db, ds, sc)
}

// ResolveOAuthCredentials loads client credentials from the shared OAuth
// connection when referenced, otherwise from ds.config.
func ResolveOAuthCredentials(ctx context.Context, db *sql.DB, ds *models.Datasource) (oauth.Credentials, error) {
	if ds.ConnectionID != nil {
		conn, err := crud.GetOAuthConnection(ctx, db, *ds.ConnectionID)
		if err != nil {
			return oauth.Credentials{}, connector.Config("%s", err.Error())
		}
		if conn == nil {
			return oauth.Credentials{}, connector.Config("oauth connection %d not found", *ds.ConnectionID)
		}
		return oauth.Credentials{ClientID: conn.ClientID, ClientSecret: conn.ClientSecret}, nil
	}
	b, err := json.Marshal(ds.Config)
	if err != nil {
		return oauth.Credentials{}, connector.Config("%s", err.Error())
	}
	var raw struct {
		ClientID     *string `json:"client_id"`
		ClientSecret *string `json:"client_secret"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return oauth.Credentials{}, connector.Config("%s", err.Error())
	}
	if raw.ClientID == nil {
		return oauth.Credentials{}, connector.Config("missing field `client_id`")
	}
	return oauth.Credentials{ClientID: *raw.ClientID, ClientSecret: raw.ClientSecret}, nil
}

func str(m map[string]any, key string) *string {
	if v, ok := m[key].(string); ok {
		return &v
	}
	return nil
}

func create(ctx context.Context, db *sql.DB, ds *models.Datasource, sourceConfig map[string]any) (connector.Connector, error) {
	switch ds.DsType {
	case models.DatasourcePostgres:
		merged := maps.Clone(ds.Config)
		if merged == nil {
			merged = map[string]any{}
		}
		maps.Copy(merged, sourceConfig)
		cfg, err := postgres.ParseConfig(merged)
		if err != nil {
			return nil, connector.Config("%s", err.Error())
		}
		return postgres.New(ctx, cfg)

	case models.DatasourceGmail:
		creds, err := ResolveOAuthCredentials(ctx, db, ds)
		if err != nil {
			return nil, err
		}
		return gmail.New(ds.ID, creds), nil

	case models.DatasourceGoogleCalendar:
		creds, err := ResolveOAuthCredentials(ctx, db, ds)
		if err != nil {
			return nil, err
		}
		return gcal.New(ds.ID, creds, str(ds.Config, "calendar_id")), nil

	case models.DatasourceGoogleDrive:
		creds, err := ResolveOAuthCredentials(ctx, db, ds)
		if err != nil {
			return nil, err
		}
		return gdrive.New(ds.ID, creds), nil

	case models.DatasourceRest:
		dsCfg, err := rest.ParseDatasourceConfig(ds.Config)
		if err != nil {
			return nil, connector.Config("%s", err.Error())
		}
		// Without a dataset (e.g. /test) fall back to a config that only
		// exercises base_url + auth.
		effective := sourceConfig
		if _, ok := sourceConfig["path"]; !ok {
			effective = map[string]any{"path": "", "schema": map[string]any{"fields": []any{}}}
		}
		dsetCfg, err := rest.ParseDatasetConfig(effective)
		if err != nil {
			return nil, connector.Config("%s", err.Error())
		}
		secret, err := rest.LoadAuthSecret(dsCfg.Auth)
		if err != nil {
			return nil, err
		}
		return rest.New(dsCfg, dsetCfg, secret)

	case models.DatasourceJira:
		creds, err := ResolveOAuthCredentials(ctx, db, ds)
		if err != nil {
			return nil, err
		}
		return jira.New(ds.ID, creds, str(ds.Config, "cloud_id")), nil

	case models.DatasourceLinear:
		entity := linear.EntityIssues
		if rt := str(sourceConfig, "resource_type"); rt != nil {
			entity = linear.ParseEntity(*rt)
		}
		// A personal API key (in the secret store under api_key_ref) wins over OAuth.
		if ref := str(ds.Config, "api_key_ref"); ref != nil {
			key, err := loadSecret(*ref)
			if err != nil {
				return nil, err
			}
			return linear.WithEntity(ds.ID, linear.APIKeyAuth(key), entity), nil
		}
		if _, err := oauth.RetrieveTokens(ds.ID); err != nil {
			hint := ""
			if _, ok := ds.Config["api_key"]; ok {
				hint = " — an inline `api_key` is present in config but was never moved " +
					"into the secret store; re-create or update this datasource through " +
					"the daemon's management API so it gets an `api_key_ref`"
			}
			return nil, connector.Config("Linear datasource %d is not authenticated: provide a personal API "+
				"key or complete the OAuth flow%s", ds.ID, hint)
		}
		creds, err := ResolveOAuthCredentials(ctx, db, ds)
		if err != nil {
			return nil, err
		}
		return linear.WithEntity(ds.ID, linear.OAuthAuth(creds), entity), nil

	case models.DatasourceGithub:
		creds, err := ResolveOAuthCredentials(ctx, db, ds)
		if err != nil {
			return nil, err
		}
		rt := github.Issues
		if v := str(sourceConfig, "resource_type"); v != nil {
			rt = github.ParseResourceType(*v)
		}
		return github.New(ds.ID, creds, str(sourceConfig, "repository"), rt, str(sourceConfig, "branch")), nil

	case models.DatasourceHubspot:
		return nil, connector.Config("HubSpot connector is not implemented yet")

	case models.DatasourceImap:
		dsCfg, err := imap.ParseDatasourceConfig(ds.Config)
		if err != nil {
			return nil, connector.Config("%s", err.Error())
		}
		password, err := loadSecret(dsCfg.PasswordRef)
		if err != nil {
			return nil, err
		}
		smtpHost := dsCfg.DefaultSMTPHost()
		if dsCfg.SMTPHost != nil {
			smtpHost = *dsCfg.SMTPHost
		}
		mailbox := "INBOX"
		if m := str(sourceConfig, "mailbox"); m != nil {
			mailbox = *m
		}
		return imap.New(ds.ID, imap.Config{
			ImapHost: dsCfg.ImapHost,
			ImapPort: dsCfg.ImapPort,
			SMTPHost: smtpHost,
			SMTPPort: dsCfg.SMTPPort,
			Username: dsCfg.Username,
			Password: password,
			Mailbox:  mailbox,
		}), nil

	case models.DatasourceFileUpload:
		return fileupload.New(), nil
	}
	return nil, connector.Config("unknown datasource type %q", fmt.Sprint(ds.DsType))
}
