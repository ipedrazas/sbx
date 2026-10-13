package factory

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ipedrazas/sbx/internal/connector"
	"github.com/ipedrazas/sbx/internal/connectors/fileupload"
	"github.com/ipedrazas/sbx/internal/connectors/gcal"
	"github.com/ipedrazas/sbx/internal/connectors/gdrive"
	"github.com/ipedrazas/sbx/internal/connectors/github"
	"github.com/ipedrazas/sbx/internal/connectors/gmail"
	"github.com/ipedrazas/sbx/internal/connectors/imap"
	"github.com/ipedrazas/sbx/internal/connectors/jira"
	"github.com/ipedrazas/sbx/internal/connectors/linear"
	"github.com/ipedrazas/sbx/internal/connectors/rest"
	"github.com/ipedrazas/sbx/internal/crud"
	"github.com/ipedrazas/sbx/internal/db"
	"github.com/ipedrazas/sbx/internal/models"
	"github.com/ipedrazas/sbx/internal/oauth"
	"github.com/ipedrazas/sbx/internal/secretstore"
)

func setup(t *testing.T) context.Context {
	t.Helper()
	secretstore.Init(secretstore.NewMemory())
	return context.Background()
}

func kindOf(err error) connector.ErrorKind {
	var ce *connector.Error
	if errors.As(err, &ce) {
		return ce.Kind
	}
	return -1
}

func TestOAuthConnectors(t *testing.T) {
	ctx := setup(t)
	cfg := map[string]any{"client_id": "cid", "client_secret": "sec", "calendar_id": "team", "cloud_id": "c1"}
	cases := []struct {
		typ   models.DatasourceType
		check func(connector.Connector) bool
	}{
		{models.DatasourceGmail, func(c connector.Connector) bool { _, ok := c.(*gmail.Connector); return ok }},
		{models.DatasourceGoogleCalendar, func(c connector.Connector) bool { _, ok := c.(*gcal.Connector); return ok }},
		{models.DatasourceGoogleDrive, func(c connector.Connector) bool { _, ok := c.(*gdrive.Connector); return ok }},
		{models.DatasourceJira, func(c connector.Connector) bool { _, ok := c.(*jira.Connector); return ok }},
		{models.DatasourceGithub, func(c connector.Connector) bool { _, ok := c.(*github.Connector); return ok }},
		{models.DatasourceFileUpload, func(c connector.Connector) bool { _, ok := c.(*fileupload.Connector); return ok }},
	}
	for _, c := range cases {
		conn, err := CreateConnectorForDataset(ctx, nil, &models.Datasource{ID: 1, DsType: c.typ, Config: cfg},
			&models.Dataset{SourceConfig: map[string]any{"repository": "o/r", "resource_type": "repo_files"}})
		if err != nil || !c.check(conn) {
			t.Errorf("%s: %T %v", c.typ, conn, err)
		}
		Close(conn)
	}
	// Missing client_id.
	if _, err := CreateConnector(ctx, nil, &models.Datasource{DsType: models.DatasourceGmail, Config: map[string]any{}}); kindOf(err) != connector.ErrConfig {
		t.Fatalf("want config error, got %v", err)
	}
}

func TestSharedConnection(t *testing.T) {
	ctx := setup(t)
	pool, err := db.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	sec := "s"
	id, err := crud.CreateOAuthConnection(ctx, pool, "google", "google", "shared-id", &sec, nil)
	if err != nil {
		t.Fatal(err)
	}
	creds, err := ResolveOAuthCredentials(ctx, pool, &models.Datasource{ConnectionID: &id})
	if err != nil || creds.ClientID != "shared-id" || *creds.ClientSecret != "s" {
		t.Fatalf("got %+v %v", creds, err)
	}
	missing := int64(999)
	if _, err := ResolveOAuthCredentials(ctx, pool, &models.Datasource{ConnectionID: &missing}); err == nil || !strings.Contains(err.Error(), "oauth connection 999 not found") {
		t.Fatalf("got %v", err)
	}
}

func TestLinear(t *testing.T) {
	ctx := setup(t)
	ds := &models.Datasource{ID: 5, DsType: models.DatasourceLinear, Config: map[string]any{"api_key": "inline"}}
	_, err := CreateConnector(ctx, nil, ds)
	if kindOf(err) != connector.ErrConfig || !strings.Contains(err.Error(), "never moved") {
		t.Fatalf("got %v", err)
	}

	_ = secretstore.Put("lin-key", []byte("k"))
	ds.Config = map[string]any{"api_key_ref": "lin-key"}
	c, err := CreateConnectorForDataset(ctx, nil, ds, &models.Dataset{SourceConfig: map[string]any{"resource_type": "teams"}})
	if _, ok := c.(*linear.Connector); !ok || err != nil {
		t.Fatalf("api key: %T %v", c, err)
	}

	ds.Config = map[string]any{"api_key_ref": "missing"}
	if _, err := CreateConnector(ctx, nil, ds); kindOf(err) != connector.ErrAuth {
		t.Fatalf("missing secret: %v", err)
	}

	ds.Config = map[string]any{"client_id": "c"}
	_ = oauth.StoreTokens(5, &oauth.Tokens{AccessToken: "a"})
	if c, err := CreateConnector(ctx, nil, ds); err != nil {
		t.Fatalf("oauth: %T %v", c, err)
	}
}

func TestRestImapAndErrors(t *testing.T) {
	ctx := setup(t)
	ds := &models.Datasource{DsType: models.DatasourceRest, Config: map[string]any{"base_url": "http://x", "auth": map[string]any{"kind": "none"}}}
	if c, err := CreateConnector(ctx, nil, ds); err != nil {
		t.Fatalf("rest: %v", err)
	} else if _, ok := c.(*rest.Connector); !ok {
		t.Fatalf("rest type %T", c)
	}
	ds.Config["auth"] = map[string]any{"kind": "bearer"}
	if _, err := CreateConnector(ctx, nil, ds); kindOf(err) != connector.ErrAuth {
		t.Fatalf("missing ref: %v", err)
	}
	if _, err := CreateConnector(ctx, nil, &models.Datasource{DsType: models.DatasourceRest, Config: map[string]any{}}); kindOf(err) != connector.ErrConfig {
		t.Fatalf("bad rest config: %v", err)
	}

	_ = secretstore.Put("imap-pw", []byte("pw"))
	ids := &models.Datasource{DsType: models.DatasourceImap, Config: map[string]any{
		"imap_host": "imap.example.com", "username": "u", "password_ref": "imap-pw"}}
	if c, err := CreateConnector(ctx, nil, ids); err != nil {
		t.Fatalf("imap: %v", err)
	} else if _, ok := c.(*imap.Connector); !ok {
		t.Fatalf("imap type %T", c)
	}
	ids.Config["password_ref"] = "nope"
	if _, err := CreateConnector(ctx, nil, ids); kindOf(err) != connector.ErrAuth {
		t.Fatalf("imap missing pw: %v", err)
	}

	if _, err := CreateConnector(ctx, nil, &models.Datasource{DsType: models.DatasourceHubspot}); kindOf(err) != connector.ErrConfig {
		t.Fatal("hubspot should be config error")
	}
	if _, err := CreateConnector(ctx, nil, &models.Datasource{DsType: "bogus"}); kindOf(err) != connector.ErrConfig {
		t.Fatal("unknown type should be config error")
	}
	if _, err := CreateConnector(ctx, nil, &models.Datasource{DsType: models.DatasourcePostgres, Config: map[string]any{}}); kindOf(err) != connector.ErrConfig {
		t.Fatalf("postgres missing conn string: %v", err)
	}
}
