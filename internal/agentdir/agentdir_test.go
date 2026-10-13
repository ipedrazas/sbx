package agentdir

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/ipedrazas/sbx/internal/actions"
	"github.com/ipedrazas/sbx/internal/clidist"
	"github.com/ipedrazas/sbx/internal/crud"
	"github.com/ipedrazas/sbx/internal/db"
	"github.com/ipedrazas/sbx/internal/models"
)

func must[T any](t *testing.T) func(T, error) T {
	t.Helper()
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	pool, err := db.OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	return pool
}

// isolate disables local-binary discovery so tests never touch host binaries.
func isolate(t *testing.T) {
	t.Helper()
	oldCand, oldCross, oldEnsure := localBinaryCandidates, crossTargetDir, ensureCLIBinary
	localBinaryCandidates = func(string) []string { return nil }
	crossTargetDir = ""
	t.Cleanup(func() {
		localBinaryCandidates, crossTargetDir, ensureCLIBinary = oldCand, oldCross, oldEnsure
	})
}

type fixture struct {
	agentID, dsID, datasetID, actionID, abID int64
}

func setup(t *testing.T, pool *sql.DB) fixture {
	t.Helper()
	ctx := context.Background()
	var f fixture
	f.agentID = must[int64](t)(crud.CreateAgent(ctx, pool, "Mail Bot", models.Ptr("Reads mail")))
	f.dsID = must[int64](t)(crud.CreateDatasource(ctx, pool, "My Gmail", "gmail", "{}", nil))
	f.datasetID = must[int64](t)(crud.CreateDataset(ctx, pool, "Inbox", f.dsID, "pull_through", nil, nil,
		models.Ptr("Recent mail"), models.Ptr("email"), nil))
	f1 := models.NewDatasetField()
	f1.SourceName, f1.DisplayName, f1.FieldType, f1.Description = "subject", "subject", "string", models.Ptr("Subject line")
	f2 := models.NewDatasetField()
	f2.SourceName, f2.DisplayName, f2.FieldType = "from", "from", "string"
	f3 := models.NewDatasetField()
	f3.SourceName, f3.DisplayName, f3.FieldType, f3.Visible = "secret", "secret", "string", false
	if err := crud.SetDatasetFields(ctx, pool, f.datasetID, []models.DatasetField{f1, f2, f3}); err != nil {
		t.Fatal(err)
	}
	must[int64](t)(crud.BindAgentDataset(ctx, pool, f.agentID, f.datasetID, "inbox", nil, nil))

	schema := map[string]any{"type": "object", "properties": map[string]any{"to": map[string]any{"type": "string"}}}
	f.actionID = must[int64](t)(actions.CreateAction(ctx, pool, f.dsID, "Send", "send_email", models.Ptr("Send <mail> & more"),
		schema, map[string]any{}, models.PolicyAlwaysApprove, map[string]any{}, models.Ptr(int64(3600))))
	f.abID = must[int64](t)(actions.BindAgentAction(ctx, pool, f.agentID, f.actionID, "send", models.Ptr("  To: {{to}}  ")))
	must[bool](t)(actions.UpdateAgentActionRateLimit(ctx, pool, f.abID, models.Ptr(int64(5)), models.Ptr(int64(60))))
	return f
}

func TestAgentSlug(t *testing.T) {
	cases := map[string]string{
		"Mail Bot":       "mail-bot",
		"  --Hello!!--":  "hello",
		"under_score-ok": "under_score-ok",
		"":               "unnamed",
		"!!!":            "unnamed",
		"Ünïcode Agent":  "ünïcode-agent",
	}
	for in, want := range cases {
		if got := AgentSlug(in); got != want {
			t.Errorf("AgentSlug(%q)=%q want %q", in, got, want)
		}
	}
	if got := AgentPath("/data", "Mail Bot"); got != filepath.Join("/data", "agents", "mail-bot") {
		t.Fatal(got)
	}
}

func TestExpandTilde(t *testing.T) {
	t.Setenv("HOME", "/home/x")
	if got := ExpandTildePub("~/skills"); got != "/home/x/skills" {
		t.Fatal(got)
	}
	if got := ExpandTildePub("~"); got != "/home/x" {
		t.Fatal(got)
	}
	if got := ExpandTildePub("~bob/x"); got != "~bob/x" {
		t.Fatal(got)
	}
	if got := ExpandTildePub("/abs"); got != "/abs" {
		t.Fatal(got)
	}
	if got := InstallTargetPath("~/s", "slug"); got != "/home/x/s/slug/SKILL.md" {
		t.Fatal(got)
	}
}

func TestBuildConfigExact(t *testing.T) {
	ctx := context.Background()
	pool := openDB(t)
	f := setup(t, pool)
	agent := must[*models.Agent](t)(crud.GetAgent(ctx, pool, f.agentID))
	abs := must[[]models.AgentAction](t)(actions.ListAgentActions(ctx, pool, f.agentID))
	cfg, err := BuildConfig(ctx, pool, agent, nil, abs, nil, "http://localhost:9820")
	if err != nil {
		t.Fatal(err)
	}
	got, err := prettyJSON(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "actions": {
    "send": {
      "action_type": "send_email",
      "approval_ttl_seconds": 3600,
      "datasource": "My Gmail",
      "description": "Send <mail> & more",
      "params_schema": {
        "properties": {
          "to": {
            "type": "string"
          }
        },
        "type": "object"
      },
      "policy_kind": "always_approve",
      "rate_limit": {
        "count": 5,
        "period_seconds": 60
      },
      "result_schema": {},
      "template": "To: {{to}}"
    }
  },
  "agent_id": 1,
  "base_url": "http://localhost:9820",
  "datasets": {},
  "token": "<YOUR_AGENT_TOKEN>"
}`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestBuildConfigDatasetsAndReadOnly(t *testing.T) {
	ctx := context.Background()
	pool := openDB(t)
	f := setup(t, pool)
	agent := must[*models.Agent](t)(crud.GetAgent(ctx, pool, f.agentID))
	bs := must[[]models.AgentDataset](t)(crud.ListAgentDatasets(ctx, pool, f.agentID))
	must[bool](t)(crud.SetDatasourceReadOnly(ctx, pool, f.dsID, true))
	abs := must[[]models.AgentAction](t)(actions.ListAgentActions(ctx, pool, f.agentID))
	cfg, err := BuildConfig(ctx, pool, agent, bs, abs, models.Ptr("tok"), "u")
	if err != nil {
		t.Fatal(err)
	}
	if cfg["token"] != "tok" {
		t.Fatal(cfg["token"])
	}
	if len(cfg["actions"].(map[string]any)) != 0 {
		t.Fatal("read-only datasource actions must be skipped")
	}
	inbox := cfg["datasets"].(map[string]any)["inbox"].(map[string]any)
	if inbox["mode"] != "pull_through" || inbox["description"] != "Recent mail" || inbox["data_kind"] != "email" {
		t.Fatalf("%v", inbox)
	}
	if _, ok := inbox["capabilities"].(map[string]any); !ok {
		t.Fatalf("capabilities missing: %v", inbox)
	}
	schema := inbox["schema"].([]any)
	if len(schema) != 2 {
		t.Fatalf("%v", schema)
	}
	s0 := schema[0].(map[string]any)
	if s0["name"] != "subject" || s0["description"] != "Subject line" || s0["searchable"] != true {
		t.Fatalf("%v", s0)
	}
	if schema[1].(map[string]any)["description"] != nil {
		t.Fatal("expected null description")
	}
	// Ensure null serialises as null.
	out, _ := prettyJSON(cfg)
	if !strings.Contains(out, `"description": null`) {
		t.Fatal(out)
	}
}

func TestBuildSkillMD(t *testing.T) {
	ctx := context.Background()
	pool := openDB(t)
	f := setup(t, pool)
	must[bool](t)(actions.SetActionState(ctx, pool, f.actionID, models.ActionPaused))
	must[bool](t)(crud.SetDatasetEnabled(ctx, pool, f.datasetID, false))
	if err := crud.SetDatasetFilters(ctx, pool, f.datasetID, []models.DatasetFilter{{
		FieldName: "from", Operator: models.OpEquals, Value: "a@b", Logic: models.LogicAnd,
	}}); err != nil {
		t.Fatal(err)
	}
	agent := must[*models.Agent](t)(crud.GetAgent(ctx, pool, f.agentID))
	bs := must[[]models.AgentDataset](t)(crud.ListAgentDatasets(ctx, pool, f.agentID))
	abs := must[[]models.AgentAction](t)(actions.ListAgentActions(ctx, pool, f.agentID))
	md, err := BuildSkillMD(ctx, pool, agent, bs, abs, "http://d:1")
	if err != nil {
		t.Fatal(err)
	}
	wantPreamble := "---\nname: mail-bot\ndescription: Reads mail\n---\n\n# SKILL: Mail Bot Agent\n\nReads mail\n\n" +
		"This agent has access to **1** dataset(s) via the DStudio API.\n\n## Available datasets\n\n" +
		"### `inbox` (Inbox)\n\nRecent mail\n\n- **Kind**: email\n- **Mode**: pull-through (live queries)\n" +
		"- **Status**: PAUSED\n- **Fields**: 2 visible\n- **Pre-filters**: 1 server-side filter(s) applied automatically\n\n" +
		"| Field | Type | Description |\n|-------|------|-------------|\n| `subject` | string | Subject line |\n| `from` | string | \u2014 |\n\n" +
		"## Available actions\n\n" +
		"Actions are **state-changing** calls (send mail, create ticket, \u2026). Each call requires an `Idempotency-Key`; safe retries replay the same response.\n\n" +
		"### `send` (My Gmail / `send_email`)\n\nSend <mail> & more\n\n" +
		"- **Policy**: always_approve \u2014 queues for human approval\n" +
		"- **Status**: Paused \u2014 calls return HTTP 422\n" +
		"- **Rate limit**: 5 call(s) per 60s\n" +
		"\n**Params schema:**\n\n```json\n{\n  \"properties\": {\n    \"to\": {\n      \"type\": \"string\"\n    }\n  },\n  \"type\": \"object\"\n}\n```\n\n" +
		"**Template** \u2014 follow this structure when drafting params. Replace placeholders with concrete values; the daemon does not substitute anything.\n\n" +
		"```\nTo: {{to}}\n```\n\n"
	if !strings.HasPrefix(md, wantPreamble) {
		t.Fatalf("preamble mismatch:\n%s", md)
	}
	rest := md[len(wantPreamble):]
	wantCLI := "## Precondition \u2014 can you execute local binaries?\n\n" +
		"Every command below spawns the local `dbxcli` binary. If your runtime cannot execute files on the user's filesystem (pure browser/API sandbox, no shell access, etc.), **stop here and tell the user**.\n\n" +
		"## CLI usage\n\nAll paths are relative to `$DBX_AGENT_DIR` (this directory). The daemon endpoint is `http://d:1`.\n\n" +
		"```bash\nexport DBX_AGENT_DIR=.\nexport DBX_CONFIG=$DBX_AGENT_DIR/config.json\n\n# List available datasets\n$DBX_AGENT_DIR/bin/dbxcli datasets\n\n" +
		"# Query the \"inbox\" dataset\n$DBX_AGENT_DIR/bin/dbxcli list inbox\n$DBX_AGENT_DIR/bin/dbxcli search inbox \"query\"\n$DBX_AGENT_DIR/bin/dbxcli get inbox <ID>\n\n" +
		"# List bound actions and their policies\n$DBX_AGENT_DIR/bin/dbxcli action list\n\n" +
		"# Invoke the \"send\" action (real execution)\n$DBX_AGENT_DIR/bin/dbxcli action invoke send --params '{...}'\n" +
		"# Or dry-run against the simulator (no external side effects):\n$DBX_AGENT_DIR/bin/dbxcli action invoke send --params '{...}' --simulated\n\n```\n\n" +
		"## Output formats\n\n"
	if !strings.HasPrefix(rest, wantCLI) {
		t.Fatalf("cli mismatch:\n%s", rest)
	}
	if !strings.HasSuffix(md, "later or contact the administrator.\n") {
		t.Fatal("footer")
	}

	pmd, err := BuildPortableSkillMD(ctx, pool, agent, bs, abs, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		"`dbxcli` and `dstudio-mcp` organised by platform and architecture). \n\n",
		"export DBX_BASE_URL=https://your-daemon-host \n\n",
		"$DBX_AGENT_DIR/$BINDIR/dbxcli action invoke send --params '{...}' --simulated\n\n",
		"There is no HTTP fallback \u2014 the CLI",
	} {
		if !strings.Contains(pmd, s) {
			t.Fatalf("portable missing %q:\n%s", s, pmd)
		}
	}
	pmd2, _ := BuildPortableSkillMD(ctx, pool, agent, bs, nil, false)
	if !strings.Contains(pmd2, "copy of `dbxcli` organised") || strings.Contains(pmd2, "Available actions") {
		t.Fatal(pmd2)
	}
}

func TestSkillMDDefaultsAndMissingDataset(t *testing.T) {
	ctx := context.Background()
	pool := openDB(t)
	agent := &models.Agent{ID: 9, Name: "No Desc"}
	md, err := BuildSkillMD(ctx, pool, agent, nil, nil, "u")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(md, "---\nname: no-desc\ndescription: Access 0 dataset(s) via the DStudio API\n---\n\n# SKILL: No Desc Agent\n\nThis agent") {
		t.Fatal(md)
	}
	_, err = BuildSkillMD(ctx, pool, agent, []models.AgentDataset{{DatasetID: 99, Alias: "x"}}, nil, "u")
	if err == nil || err.Error() != "dataset not found" {
		t.Fatal(err)
	}
	// Missing action is skipped; missing dataset in config yields schema-only entry.
	cfg, err := BuildConfig(ctx, pool, agent, []models.AgentDataset{{DatasetID: 99, Alias: "x"}},
		[]models.AgentAction{{ActionID: 42, Alias: "gone"}}, nil, "u")
	if err != nil {
		t.Fatal(err)
	}
	x := cfg["datasets"].(map[string]any)["x"].(map[string]any)
	if len(x) != 1 || len(cfg["actions"].(map[string]any)) != 0 {
		t.Fatal(cfg)
	}
	out, _ := prettyJSON(cfg)
	if !strings.Contains(out, `"schema": []`) {
		t.Fatal(out)
	}
}

func TestInstallSkillTo(t *testing.T) {
	root := t.TempDir()
	// Legacy layout with only SKILL.md → removed entirely.
	if err := os.MkdirAll(filepath.Join(root, "dstudio"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "dstudio", "SKILL.md"), []byte("old"), 0o644)
	target, err := InstallSkillTo(root, "mail-bot", "new")
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != "new" || target != filepath.Join(root, "mail-bot", "SKILL.md") {
		t.Fatal(target)
	}
	if _, err := os.Stat(filepath.Join(root, "dstudio")); !os.IsNotExist(err) {
		t.Fatal("legacy dir should be removed")
	}
	// Legacy dir with other files is kept.
	os.MkdirAll(filepath.Join(root, "dstudio"), 0o755)
	os.WriteFile(filepath.Join(root, "dstudio", "SKILL.md"), []byte("old"), 0o644)
	os.WriteFile(filepath.Join(root, "dstudio", "mine.txt"), []byte("x"), 0o644)
	if _, err := InstallSkillTo(root, "mail-bot", "new"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "dstudio", "mine.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "dstudio", "SKILL.md")); !os.IsNotExist(err) {
		t.Fatal("legacy SKILL.md should be removed")
	}
	// Unwritable target.
	blocker := filepath.Join(root, "file")
	os.WriteFile(blocker, []byte("x"), 0o644)
	if _, err := InstallSkillTo(blocker, "s", "x"); err == nil {
		t.Fatal("expected error")
	}
}

func TestSyncAgentDir(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	pool := openDB(t)
	f := setup(t, pool)
	dataDir := t.TempDir()

	// Warm the dbxcli cache for the host and linux platforms.
	ver := clidist.CLIVersion()
	for _, dir := range []string{"linux-x86_64", "linux-aarch64"} {
		p := clidist.PlatformByDir(dir)
		writeCached(t, dataDir, ver, p)
	}
	if hp := clidist.HostPlatform(); hp != nil {
		writeCached(t, dataDir, ver, hp)
	}
	// Local dstudio-mcp + cross build.
	local := t.TempDir()
	os.WriteFile(filepath.Join(local, "dstudio-mcp"), []byte("mcp"), 0o644)
	localBinaryCandidates = func(name string) []string { return []string{filepath.Join(local, name)} }
	crossTargetDir = t.TempDir()
	os.MkdirAll(filepath.Join(crossTargetDir, "x86_64-unknown-linux-gnu", "release"), 0o755)
	os.WriteFile(filepath.Join(crossTargetDir, "x86_64-unknown-linux-gnu", "release", "dstudio-mcp"), []byte("x"), 0o644)

	// Installation target + legacy skills dir.
	installRoot := t.TempDir()
	must[int64](t)(crud.UpsertAgentInstallation(ctx, pool, f.agentID, installRoot))
	agentDir := AgentPath(dataDir, "Mail Bot")
	os.MkdirAll(filepath.Join(agentDir, "skills", "dstudio"), 0o755)

	dir, err := SyncAgentDir(ctx, pool, dataDir, f.agentID, models.Ptr("secret-token"), "http://x")
	if err != nil {
		t.Fatal(err)
	}
	if dir != agentDir {
		t.Fatal(dir)
	}
	var cfg map[string]any
	b := must[[]byte](t)(os.ReadFile(filepath.Join(dir, "config.json")))
	if err := json.Unmarshal(b, &cfg); err != nil || cfg["token"] != "secret-token" {
		t.Fatal(string(b))
	}
	skill := must[[]byte](t)(os.ReadFile(filepath.Join(dir, "skills", "mail-bot", "SKILL.md")))
	if _, err := os.Stat(filepath.Join(dir, "skills", "dstudio")); !os.IsNotExist(err) {
		t.Fatal("legacy skills dir should be gone")
	}
	installed := must[[]byte](t)(os.ReadFile(filepath.Join(installRoot, "mail-bot", "SKILL.md")))
	if string(installed) != string(skill) {
		t.Fatal("installation not refreshed")
	}
	for _, p := range []string{"bin/linux-amd64/dbxcli", "bin/linux-arm64/dbxcli", "bin/dstudio-mcp", "bin/linux-amd64/dstudio-mcp"} {
		st, err := os.Stat(filepath.Join(dir, p))
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if st.Mode().Perm() != 0o755 {
			t.Fatalf("%s mode %v", p, st.Mode())
		}
	}
	if clidist.HostPlatform() != nil {
		if _, err := os.Stat(filepath.Join(dir, "bin", "dbxcli")); err != nil {
			t.Fatal(err)
		}
	}

	RemoveAgentDir(dataDir, "Mail Bot")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("dir should be removed")
	}
	if _, err := SyncAgentDir(ctx, pool, dataDir, 999, nil, "u"); err == nil || err.Error() != "agent not found" {
		t.Fatal(err)
	}
	if FindExistingTokenForConfig(ctx, pool, f.agentID) != nil {
		t.Fatal("expected nil")
	}
}

func TestRefreshInstallationsBadTarget(t *testing.T) {
	ctx := context.Background()
	pool := openDB(t)
	f := setup(t, pool)
	blocker := filepath.Join(t.TempDir(), "file")
	os.WriteFile(blocker, []byte("x"), 0o644)
	good := t.TempDir()
	must[int64](t)(crud.UpsertAgentInstallation(ctx, pool, f.agentID, blocker))
	must[int64](t)(crud.UpsertAgentInstallation(ctx, pool, f.agentID, good))
	if err := RefreshInstallations(ctx, pool, f.agentID, "mail-bot", "md"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(good, "mail-bot", "SKILL.md")); string(b) != "md" {
		t.Fatal("good target not written")
	}
}

func writeCached(t *testing.T, dataDir, ver string, p *clidist.Platform) {
	t.Helper()
	d := filepath.Join(dataDir, "bin", ver, p.Dir)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, p.Exe), []byte("bin-"+p.Dir), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := clidist.CachedCLIBinary(dataDir, ver, p); !ok {
		t.Fatal("cache layout mismatch")
	}
}

func TestExportSkillPackage(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	pool := openDB(t)
	f := setup(t, pool)
	dataDir := t.TempDir()
	out := t.TempDir()
	ver := clidist.CLIVersion()
	for i := range clidist.Platforms {
		writeCached(t, dataDir, ver, &clidist.Platforms[i])
	}
	local := t.TempDir()
	os.WriteFile(filepath.Join(local, "dstudio-mcp"), []byte("mcp"), 0o755)
	localBinaryCandidates = func(name string) []string {
		return []string{filepath.Join(local, "missing"), filepath.Join(local, name)}
	}

	pkg, zipPath, err := ExportSkillPackage(ctx, pool, dataDir, f.agentID, nil, "http://d", out, &ExportOptions{IncludeMCP: true, Zip: true})
	if err != nil {
		t.Fatal(err)
	}
	if pkg != filepath.Join(out, "mail-bot") || zipPath == nil || *zipPath != filepath.Join(out, "mail-bot.zip") {
		t.Fatal(pkg, zipPath)
	}
	for i := range clidist.Platforms {
		p := clidist.Platforms[i]
		b, err := os.ReadFile(filepath.Join(pkg, "scripts", p.Dir, p.Exe))
		if err != nil || string(b) != "bin-"+p.Dir {
			t.Fatalf("%s: %v %q", p.Dir, err, b)
		}
	}
	skill := must[[]byte](t)(os.ReadFile(filepath.Join(pkg, "SKILL.md")))
	if !strings.Contains(string(skill), "`dstudio-mcp`") {
		t.Fatal("expected mcp mention")
	}

	zr, err := zip.OpenReader(*zipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	var names []string
	for _, zf := range zr.File {
		names = append(names, zf.Name)
		if zf.Name == "mail-bot/scripts/linux-x86_64/dbxcli" && zf.Mode().Perm() != 0o755 {
			t.Fatalf("mode %v", zf.Mode())
		}
		if zf.Method != zip.Deflate {
			t.Fatal("expected deflate")
		}
	}
	sort.Strings(names)
	for _, want := range []string{"mail-bot/SKILL.md", "mail-bot/config.json", "mail-bot/scripts/windows-x86_64/dbxcli.exe"} {
		if i := sort.SearchStrings(names, want); i >= len(names) || names[i] != want {
			t.Fatalf("missing %s in %v", want, names)
		}
	}
	if hp := clidist.HostPlatform(); hp != nil {
		if _, err := os.Stat(filepath.Join(pkg, "scripts", hp.Dir, "dstudio-mcp")); err != nil {
			t.Fatal(err)
		}
	}
}

func TestExportDownloadFailures(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	pool := openDB(t)
	f := setup(t, pool)
	local := t.TempDir()
	os.WriteFile(filepath.Join(local, "dbxcli"), []byte("local"), 0o755)
	localBinaryCandidates = func(name string) []string { return []string{filepath.Join(local, name)} }
	ensureCLIBinary = func(context.Context, string, string, *clidist.Platform) (string, error) {
		return "", errors.New("offline")
	}
	out := t.TempDir()
	pkg, zipPath, err := ExportSkillPackage(ctx, pool, t.TempDir(), f.agentID, models.Ptr("t"), "u", out, &ExportOptions{})
	if err != nil || zipPath != nil {
		t.Fatal(err, zipPath)
	}
	if hp := clidist.HostPlatform(); hp != nil {
		b, err := os.ReadFile(filepath.Join(pkg, "scripts", hp.Dir, "dbxcli"))
		if err != nil || string(b) != "local" {
			t.Fatal(err, string(b))
		}
	}
	if _, err := os.Stat(filepath.Join(pkg, "scripts", "windows-x86_64")); !os.IsNotExist(err) {
		t.Fatal("non-host platforms should be skipped")
	}
	if _, _, err := ExportSkillPackage(ctx, pool, t.TempDir(), 404, nil, "u", out, &ExportOptions{}); err == nil || err.Error() != "agent not found" {
		t.Fatal(err)
	}
}

func TestRustNames(t *testing.T) {
	if rustOS() == "darwin" || rustArch() == "amd64" || rustArch() == "arm64" {
		t.Fatal(rustOS(), rustArch())
	}
}
