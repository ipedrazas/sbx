// Package agentdir manages on-disk agent directories at `<data_dir>/agents/<slug>/`.
//
// Each agent gets a self-contained folder with:
//   - `config.json` — CLI/MCP config (base_url, agent_id, token, datasets, actions)
//   - `skills/<slug>/SKILL.md` — agent-aware skill doc with frontmatter
//   - `bin/` — copies of the dbxcli and dstudio-mcp binaries
package agentdir

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/ipedrazas/sbx/internal/actions"
	"github.com/ipedrazas/sbx/internal/capabilities"
	"github.com/ipedrazas/sbx/internal/clidist"
	"github.com/ipedrazas/sbx/internal/crud"
	"github.com/ipedrazas/sbx/internal/models"
	"github.com/ipedrazas/sbx/internal/util"
)

// TokenPlaceholder is written into config.json when no plaintext token is known.
const TokenPlaceholder = "<YOUR_AGENT_TOKEN>"

// AgentsRoot returns the root directory for all agent directories.
func AgentsRoot(dataDir string) string {
	return filepath.Join(dataDir, "agents")
}

// AgentSlug sanitizes an agent's display name into a filesystem-safe slug. The
// same slug is the skill directory name so harnesses register one skill per agent.
func AgentSlug(agentName string) string {
	var b strings.Builder
	for _, c := range agentName {
		if unicode.IsLetter(c) || unicode.IsNumber(c) || c == '-' || c == '_' {
			b.WriteRune(c)
		} else {
			b.WriteRune('-')
		}
	}
	safe := strings.ToLower(strings.Trim(b.String(), "-"))
	if safe == "" {
		return "unnamed"
	}
	return safe
}

// AgentPath returns the directory path for a specific agent.
func AgentPath(dataDir, agentName string) string {
	return filepath.Join(AgentsRoot(dataDir), AgentSlug(agentName))
}

// prettyJSON mirrors serde_json::to_string_pretty: 2-space indent, sorted keys,
// no HTML escaping.
func prettyJSON(v any) (string, error) {
	raw, err := util.MarshalJSON(v)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// SyncAgentDir regenerates the full agent directory (config.json, SKILL.md,
// bin/ copies) and returns its path. A nil tokenPlaintext writes the
// `<YOUR_AGENT_TOKEN>` placeholder.
func SyncAgentDir(ctx context.Context, db *sql.DB, dataDir string, agentID int64, tokenPlaintext *string, daemonURL string) (string, error) {
	agent, err := crud.GetAgent(ctx, db, agentID)
	if err != nil {
		return "", err
	}
	if agent == nil {
		return "", errors.New("agent not found")
	}

	dir := AgentPath(dataDir, agent.Name)
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", fmt.Errorf("failed to create agent dir: %v", err)
	}

	bindings, err := crud.ListAgentDatasets(ctx, db, agentID)
	if err != nil {
		return "", err
	}
	actionBindings, err := actions.ListAgentActions(ctx, db, agentID)
	if err != nil {
		return "", err
	}

	// config.json
	config, err := BuildConfig(ctx, db, agent, bindings, actionBindings, tokenPlaintext, daemonURL)
	if err != nil {
		return "", err
	}
	configStr, err := prettyJSON(config)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(configStr), 0o644); err != nil {
		return "", fmt.Errorf("failed to write config.json: %v", err)
	}

	// skills/<slug>/SKILL.md — dir named after the agent so skill loaders
	// (which expect dir name == frontmatter name) don't collide.
	slug := AgentSlug(agent.Name)
	skillDir := filepath.Join(dir, "skills", slug)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		return "", fmt.Errorf("failed to create skills dir: %v", err)
	}
	// Clean up the pre-migration shared `skills/dstudio/` dir.
	legacyDir := filepath.Join(dir, "skills", "dstudio")
	if legacyDir != skillDir {
		if _, err := os.Stat(legacyDir); err == nil {
			_ = os.RemoveAll(legacyDir)
		}
	}
	skill, err := BuildSkillMD(ctx, db, agent, bindings, actionBindings, daemonURL)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skill), 0o644); err != nil {
		return "", fmt.Errorf("failed to write SKILL.md: %v", err)
	}

	// bin/ copies (real copies, not symlinks, so container mounts work).
	// dbxcli: reuse whatever the export flow already cached — no network here.
	cliVer := clidist.CLIVersion()
	src, ok := "", false
	if hp := clidist.HostPlatform(); hp != nil {
		src, ok = clidist.CachedCLIBinary(dataDir, cliVer, hp)
	}
	if ok {
		copyIntoScripts(binDir, "", "dbxcli", src)
	} else {
		copyBinary(binDir, "dbxcli")
	}
	for _, pair := range [][2]string{
		{"linux-amd64", "linux-x86_64"},
		{"linux-arm64", "linux-aarch64"},
	} {
		if p := clidist.PlatformByDir(pair[1]); p != nil {
			if src, ok := clidist.CachedCLIBinary(dataDir, cliVer, p); ok {
				copyIntoScripts(binDir, pair[0], "dbxcli", src)
			}
		}
	}

	// MCP: copied when present, skipped otherwise.
	copyBinary(binDir, "dstudio-mcp")
	copyCrossBinary(binDir, "linux-amd64", "x86_64-unknown-linux-gnu", "dstudio-mcp")
	copyCrossBinary(binDir, "linux-arm64", "aarch64-unknown-linux-gnu", "dstudio-mcp")

	// External installations: keep installed copies in sync. Failures are
	// logged but don't fail the sync.
	if err := RefreshInstallations(ctx, db, agentID, slug, skill); err != nil {
		slog.Warn(fmt.Sprintf("failed to refresh skill installations for agent %d: %v", agentID, err))
	}

	return dir, nil
}

// ExpandTildePub expands a leading `~` or `~/` to $HOME (public entry point).
func ExpandTildePub(path string) string {
	return expandTilde(path)
}

// expandTilde expands a leading `~` or `~/` to $HOME. Anything more exotic
// (e.g. `~user`) is returned unchanged.
func expandTilde(path string) string {
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		if home, ok := os.LookupEnv("HOME"); ok {
			return filepath.Join(home, rest)
		}
	}
	if path == "~" {
		if home, ok := os.LookupEnv("HOME"); ok {
			return home
		}
	}
	return path
}

// InstallTargetPath resolves a user-supplied install folder (the parent, e.g.
// `~/.hermes/skills`) to `<folder>/<slug>/SKILL.md`.
func InstallTargetPath(folderPath, slug string) string {
	return filepath.Join(expandTilde(folderPath), slug, "SKILL.md")
}

// InstallSkillTo writes SKILL.md into a single installation target, creating
// `<slug>/` if missing, and returns the written path.
func InstallSkillTo(folderPath, slug, skillMD string) (string, error) {
	target := InstallTargetPath(folderPath, slug)
	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", fmt.Errorf("failed to create %s: %v", parent, err)
	}
	if err := os.WriteFile(target, []byte(skillMD), 0o644); err != nil {
		return "", fmt.Errorf("failed to write %s: %v", target, err)
	}

	// Clean up the legacy `dstudio/SKILL.md` sibling from the pre-slug layout.
	if slug != "dstudio" {
		legacyDir := filepath.Join(expandTilde(folderPath), "dstudio")
		_ = os.Remove(filepath.Join(legacyDir, "SKILL.md"))
		// Only remove the legacy dir if it's now empty — never nuke user files.
		if entries, err := os.ReadDir(legacyDir); err == nil && len(entries) == 0 {
			_ = os.Remove(legacyDir)
		}
	}
	return target, nil
}

// RefreshInstallations rewrites every tracked installation for an agent.
// Individual write failures are logged and don't abort the batch.
func RefreshInstallations(ctx context.Context, db *sql.DB, agentID int64, slug, skillMD string) error {
	installs, err := crud.ListAgentInstallations(ctx, db, agentID)
	if err != nil {
		return err
	}
	for _, inst := range installs {
		if _, err := InstallSkillTo(inst.FolderPath, slug, skillMD); err != nil {
			slog.Warn(fmt.Sprintf("skill install refresh failed for agent %d → %s: %v", agentID, inst.FolderPath, err))
			continue
		}
		_ = crud.TouchAgentInstallation(ctx, db, inst.ID)
	}
	return nil
}

// RemoveAgentDir removes an agent directory entirely.
func RemoveAgentDir(dataDir, agentName string) {
	_ = os.RemoveAll(AgentPath(dataDir, agentName))
}

// resolveActionDatasource loads an action binding's action and datasource
// name. skip is true when the action is missing or its datasource is read-only.
func resolveActionDatasource(ctx context.Context, db *sql.DB, ab *models.AgentAction) (action *models.Action, dsName string, skip bool, err error) {
	action, err = actions.GetAction(ctx, db, ab.ActionID)
	if err != nil {
		return nil, "", false, err
	}
	if action == nil {
		return nil, "", true, nil
	}
	ds, err := crud.GetDatasource(ctx, db, action.DatasourceID)
	if err != nil {
		return nil, "", false, err
	}
	if ds != nil && ds.ReadOnly {
		return nil, "", true, nil
	}
	if ds != nil {
		dsName = ds.Name
	} else {
		dsName = fmt.Sprintf("ds#%d", action.DatasourceID)
	}
	return action, dsName, false, nil
}

// BuildConfig builds the config.json content.
func BuildConfig(ctx context.Context, db *sql.DB, agent *models.Agent, bindings []models.AgentDataset,
	actionBindings []models.AgentAction, tokenPlaintext *string, daemonURL string) (map[string]any, error) {
	datasets := map[string]any{}

	for _, binding := range bindings {
		dataset, err := crud.GetDataset(ctx, db, binding.DatasetID)
		if err != nil {
			return nil, err
		}
		fields, err := crud.GetDatasetFields(ctx, db, binding.DatasetID)
		if err != nil {
			return nil, err
		}
		// Datasource type drives effective sortability; nil when unloadable,
		// in which case nothing is advertised as sortable.
		var dsType *models.DatasourceType
		if dataset != nil {
			ds, err := crud.GetDatasource(ctx, db, dataset.DatasourceID)
			if err != nil {
				return nil, err
			}
			if ds != nil {
				t := ds.DsType
				dsType = &t
			}
		}

		visible := []any{}
		for i := range fields {
			f := &fields[i]
			if !f.Visible {
				continue
			}
			sortable := false
			if dataset != nil && dsType != nil {
				sortable = capabilities.FieldSortable(dataset, *dsType, f)
			}
			var desc any
			if f.Description != nil {
				desc = *f.Description
			}
			visible = append(visible, map[string]any{
				"name":        f.DisplayName,
				"type":        f.FieldType,
				"description": desc,
				"searchable":  f.Searchable,
				"sortable":    sortable,
			})
		}

		entry := map[string]any{"schema": visible}
		if dataset != nil {
			entry["mode"] = configModeString(dataset.Mode)
			if dataset.Description != nil {
				entry["description"] = *dataset.Description
			}
			if dataset.DataKind != nil {
				entry["data_kind"] = *dataset.DataKind
			}
			if dsType != nil {
				entry["capabilities"] = capabilities.Compute(dataset, *dsType, fields).ToJSON()
			}
		}
		datasets[binding.Alias] = entry
	}

	// Actions map: alias → descriptor so the CLI / MCP can present invocation
	// tools without re-querying the daemon.
	actionMap := map[string]any{}
	for i := range actionBindings {
		ab := &actionBindings[i]
		action, dsName, skip, err := resolveActionDatasource(ctx, db, ab)
		if err != nil {
			return nil, err
		}
		if skip {
			continue
		}
		policyKind := string(action.PolicyKind)
		if policyKind == "" {
			policyKind = "auto"
		}
		entry := map[string]any{
			"action_type": action.ActionType,
			"datasource":  dsName,
			"policy_kind": policyKind,
		}
		if action.ApprovalTTLSeconds != nil {
			entry["approval_ttl_seconds"] = *action.ApprovalTTLSeconds
		}
		if action.ParamsSchema != nil {
			entry["params_schema"] = action.ParamsSchema
		}
		if action.ResultSchema != nil {
			entry["result_schema"] = action.ResultSchema
		}
		if action.Description != nil {
			entry["description"] = *action.Description
		}
		if ab.RateLimitCount != nil && ab.RateLimitPeriodSeconds != nil {
			entry["rate_limit"] = map[string]any{
				"count":          *ab.RateLimitCount,
				"period_seconds": *ab.RateLimitPeriodSeconds,
			}
		}
		if ab.Template != nil {
			if trimmed := strings.TrimSpace(*ab.Template); trimmed != "" {
				entry["template"] = trimmed
			}
		}
		actionMap[ab.Alias] = entry
	}

	token := TokenPlaceholder
	if tokenPlaintext != nil {
		token = *tokenPlaintext
	}

	return map[string]any{
		"base_url": daemonURL,
		"agent_id": agent.ID,
		"token":    token,
		"datasets": datasets,
		"actions":  actionMap,
	}, nil
}

func configModeString(m models.DatasetMode) string {
	switch m {
	case models.ModePullThrough:
		return "pull_through"
	case models.ModeMirror:
		return "mirror"
	case models.ModeSynthetic:
		return "synthetic"
	default:
		return "cached"
	}
}

func skillModeString(m models.DatasetMode) string {
	switch m {
	case models.ModePullThrough:
		return "pull-through (live queries)"
	case models.ModeMirror:
		return "mirror (offline snapshot)"
	case models.ModeSynthetic:
		return "synthetic (generated data)"
	default:
		return "cached"
	}
}

func policyDescription(p models.PolicyKind) string {
	switch p {
	case models.PolicyAlwaysApprove:
		return "always_approve — queues for human approval"
	case models.PolicyThreshold:
		return "threshold — queues unless conditions met"
	default:
		return "auto — executes immediately"
	}
}

// actionStateDebug mirrors Rust's `{:?}` on ActionState.
func actionStateDebug(s models.ActionState) string {
	switch s {
	case models.ActionPaused:
		return "Paused"
	case models.ActionDisabled:
		return "Disabled"
	default:
		return "Enabled"
	}
}

// buildSkillMDPreamble builds the front-matter + datasets + actions sections
// shared by the local and portable SKILL.md variants.
func buildSkillMDPreamble(ctx context.Context, db *sql.DB, agent *models.Agent, bindings []models.AgentDataset,
	actionBindings []models.AgentAction) (string, error) {
	var md strings.Builder

	skillName := strings.ReplaceAll(strings.ToLower(agent.Name), " ", "-")
	description := fmt.Sprintf("Access %d dataset(s) via the DStudio API", len(bindings))
	if agent.Description != nil {
		description = *agent.Description
	}
	md.WriteString("---\n")
	fmt.Fprintf(&md, "name: %s\n", skillName)
	fmt.Fprintf(&md, "description: %s\n", description)
	md.WriteString("---\n\n")

	fmt.Fprintf(&md, "# SKILL: %s Agent\n\n", agent.Name)
	if agent.Description != nil {
		fmt.Fprintf(&md, "%s\n\n", *agent.Description)
	}
	fmt.Fprintf(&md, "This agent has access to **%d** dataset(s) via the DStudio API.\n\n", len(bindings))

	md.WriteString("## Available datasets\n\n")

	for _, binding := range bindings {
		dataset, err := crud.GetDataset(ctx, db, binding.DatasetID)
		if err != nil {
			return "", err
		}
		if dataset == nil {
			return "", errors.New("dataset not found")
		}
		fields, err := crud.GetDatasetFields(ctx, db, binding.DatasetID)
		if err != nil {
			return "", err
		}
		filters, err := crud.GetDatasetFilters(ctx, db, binding.DatasetID)
		if err != nil {
			return "", err
		}
		var visible []models.DatasetField
		for _, f := range fields {
			if f.Visible {
				visible = append(visible, f)
			}
		}

		fmt.Fprintf(&md, "### `%s` (%s)\n\n", binding.Alias, dataset.Name)
		if dataset.Description != nil {
			fmt.Fprintf(&md, "%s\n\n", *dataset.Description)
		}
		if dataset.DataKind != nil {
			fmt.Fprintf(&md, "- **Kind**: %s\n", *dataset.DataKind)
		}
		fmt.Fprintf(&md, "- **Mode**: %s\n", skillModeString(dataset.Mode))
		if !dataset.Enabled {
			md.WriteString("- **Status**: PAUSED\n")
		}
		fmt.Fprintf(&md, "- **Fields**: %d visible\n", len(visible))
		if len(filters) > 0 {
			fmt.Fprintf(&md, "- **Pre-filters**: %d server-side filter(s) applied automatically\n", len(filters))
		}
		md.WriteString("\n")

		if len(visible) > 0 {
			md.WriteString("| Field | Type | Description |\n")
			md.WriteString("|-------|------|-------------|\n")
			for _, f := range visible {
				desc := "—"
				if f.Description != nil {
					desc = *f.Description
				}
				fmt.Fprintf(&md, "| `%s` | %s | %s |\n", f.DisplayName, f.FieldType, desc)
			}
			md.WriteString("\n")
		}
	}

	if len(actionBindings) > 0 {
		md.WriteString("## Available actions\n\n")
		md.WriteString("Actions are **state-changing** calls (send mail, create ticket, …). " +
			"Each call requires an `Idempotency-Key`; safe retries replay the same response.\n\n")
		for i := range actionBindings {
			ab := &actionBindings[i]
			action, dsName, skip, err := resolveActionDatasource(ctx, db, ab)
			if err != nil {
				return "", err
			}
			if skip {
				continue
			}
			fmt.Fprintf(&md, "### `%s` (%s / `%s`)\n\n", ab.Alias, dsName, action.ActionType)
			if action.Description != nil {
				fmt.Fprintf(&md, "%s\n\n", *action.Description)
			}
			fmt.Fprintf(&md, "- **Policy**: %s\n", policyDescription(action.PolicyKind))
			if action.State == models.ActionPaused || action.State == models.ActionDisabled {
				fmt.Fprintf(&md, "- **Status**: %s — calls return HTTP 422\n", actionStateDebug(action.State))
			}
			if ab.RateLimitCount != nil && ab.RateLimitPeriodSeconds != nil {
				fmt.Fprintf(&md, "- **Rate limit**: %d call(s) per %ds\n", *ab.RateLimitCount, *ab.RateLimitPeriodSeconds)
			}
			if action.ParamsSchema != nil {
				md.WriteString("\n**Params schema:**\n\n")
				md.WriteString("```json\n")
				s, err := prettyJSON(action.ParamsSchema)
				if err != nil {
					s = "{}"
				}
				md.WriteString(s)
				md.WriteString("\n```\n\n")
			} else {
				md.WriteString("\n")
			}

			if ab.Template != nil {
				if trimmed := strings.TrimSpace(*ab.Template); trimmed != "" {
					md.WriteString("**Template** — follow this structure when drafting params. Replace placeholders with concrete values; the daemon does not substitute anything.\n\n")
					md.WriteString("```\n")
					md.WriteString(trimmed)
					md.WriteString("\n```\n\n")
				}
			}
		}
	}

	return md.String(), nil
}

// appendSkillMDFooter appends the output-formats and paused-datasets sections.
func appendSkillMDFooter(md *strings.Builder) {
	md.WriteString("## Output formats\n\n")
	md.WriteString("Add `--format <fmt>` to any command:\n\n")
	md.WriteString("- `json` (default) — pretty-printed JSON\n")
	md.WriteString("- `text` — aligned plain-text table\n")
	md.WriteString("- `csv` — RFC 4180 CSV\n\n")

	md.WriteString("## Paused datasets\n\n")
	md.WriteString("A dataset can be temporarily paused by its administrator. When a dataset is paused, " +
		"all queries to it return **HTTP 503 Service Unavailable** with the message " +
		"`\"dataset is paused\"`. This is a temporary state — the dataset has not been " +
		"deleted and will become available again once resumed.\n\n")
	md.WriteString("If you encounter a 503 error on a dataset, **do not retry in a loop**. Instead, " +
		"report to the user that the dataset is currently paused and suggest they check back " +
		"later or contact the administrator.\n")
}

// writeCLIExamples writes the per-dataset and per-action command examples.
func writeCLIExamples(md *strings.Builder, bin string, bindings []models.AgentDataset, actionBindings []models.AgentAction) {
	for _, b := range bindings {
		fmt.Fprintf(md, "# Query the \"%s\" dataset\n", b.Alias)
		fmt.Fprintf(md, "%s list %s\n", bin, b.Alias)
		fmt.Fprintf(md, "%s search %s \"query\"\n", bin, b.Alias)
		fmt.Fprintf(md, "%s get %s <ID>\n\n", bin, b.Alias)
	}
	if len(actionBindings) > 0 {
		md.WriteString("# List bound actions and their policies\n")
		fmt.Fprintf(md, "%s action list\n\n", bin)
		for _, ab := range actionBindings {
			fmt.Fprintf(md, "# Invoke the \"%s\" action (real execution)\n", ab.Alias)
			fmt.Fprintf(md, "%s action invoke %s --params '{...}'\n", bin, ab.Alias)
			md.WriteString("# Or dry-run against the simulator (no external side effects):\n")
			fmt.Fprintf(md, "%s action invoke %s --params '{...}' --simulated\n\n", bin, ab.Alias)
		}
	}
	md.WriteString("```\n\n")
}

// BuildSkillMD builds SKILL.md content with frontmatter (local/installed variant).
func BuildSkillMD(ctx context.Context, db *sql.DB, agent *models.Agent, bindings []models.AgentDataset,
	actionBindings []models.AgentAction, daemonURL string) (string, error) {
	pre, err := buildSkillMDPreamble(ctx, db, agent, bindings, actionBindings)
	if err != nil {
		return "", err
	}
	var md strings.Builder
	md.WriteString(pre)

	md.WriteString("## Precondition — can you execute local binaries?\n\n")
	md.WriteString("Every command below spawns the local `dbxcli` binary. " +
		"If your runtime cannot execute files on the user's filesystem " +
		"(pure browser/API sandbox, no shell access, etc.), **stop here " +
		"and tell the user**.\n\n")

	md.WriteString("## CLI usage\n\n")
	fmt.Fprintf(&md, "All paths are relative to `$DBX_AGENT_DIR` (this directory). "+
		"The daemon endpoint is `%s`.\n\n", daemonURL)
	md.WriteString("```bash\n")
	md.WriteString("export DBX_AGENT_DIR=.\n")
	md.WriteString("export DBX_CONFIG=$DBX_AGENT_DIR/config.json\n\n")
	md.WriteString("# List available datasets\n")
	md.WriteString("$DBX_AGENT_DIR/bin/dbxcli datasets\n\n")
	writeCLIExamples(&md, "$DBX_AGENT_DIR/bin/dbxcli", bindings, actionBindings)

	appendSkillMDFooter(&md)
	return md.String(), nil
}

// BuildPortableSkillMD builds SKILL.md for the portable/export package format:
// `$DBX_AGENT_DIR=.`, `$DBX_BASE_URL` for remote daemon access, and
// `scripts/<platform>/dbxcli` with platform auto-detection.
func BuildPortableSkillMD(ctx context.Context, db *sql.DB, agent *models.Agent, bindings []models.AgentDataset,
	actionBindings []models.AgentAction, includeMCP bool) (string, error) {
	pre, err := buildSkillMDPreamble(ctx, db, agent, bindings, actionBindings)
	if err != nil {
		return "", err
	}
	var md strings.Builder
	md.WriteString(pre)

	md.WriteString("## Precondition — can you execute local binaries?\n\n")
	md.WriteString("Every command below spawns the local `dbxcli` binary that lives " +
		"in this agent's directory. If your runtime cannot execute files on " +
		"the user's filesystem (pure browser/API sandbox, no shell access, " +
		"etc.), **stop here and tell the user**. There is no HTTP fallback — " +
		"the CLI is the only way this agent's data is reachable, and retrying " +
		"in a loop will not change that.\n\n")

	md.WriteString("## CLI usage\n\n")
	binContents := "`dbxcli` organised by platform and architecture"
	if includeMCP {
		binContents = "`dbxcli` and `dstudio-mcp` organised by platform and architecture"
	}
	fmt.Fprintf(&md, "Every command is rooted at `$DBX_AGENT_DIR`, an environment "+
		"variable you export once per session.\n\n"+
		"Inside the SKILL directory you'll find `config.json` (agent id, token, "+
		"dataset/action metadata) and `scripts` (a self-contained copy of "+
		"%s). \n\n", binContents)

	md.WriteString("### Pitfall: binary path and base URL when running locally\n\n")
	md.WriteString("The skill directory has binaries under `scripts/<platform>/` " +
		"(e.g. `scripts/darwin-arm64/dbxcli`), NOT directly under " +
		"`$DBX_AGENT_DIR/$BINDIR/`. The `$BINDIR` path in the skill docs " +
		"points to a non-existent location — use `scripts/$BINDIR/` instead.\n\n" +
		"Override endpoint setting the `DBX_BASE_URL` env var:\n\n" +
		"```bash\n" +
		"export DBX_BASE_URL=https://your-daemon-host\n" +
		"```\n\n")

	md.WriteString("```bash\n")
	md.WriteString("# One-time per session: point the CLI at this agent.\n")
	md.WriteString("export DBX_AGENT_DIR=. # Skill directory\n")
	md.WriteString("export DBX_CONFIG=$DBX_AGENT_DIR/config.json\n")
	md.WriteString("export DBX_BASE_URL=https://your-daemon-host \n\n")
	md.WriteString("export BINDIR=scripts/$(echo \"$(uname)-$(uname -m)\" | tr '[:upper:]' '[:lower:]')\n\n")
	md.WriteString("# Windows has no `uname`: use `scripts/windows-x86_64/dbxcli.exe` directly.\n\n")
	md.WriteString("# List available datasets\n")
	md.WriteString("$DBX_AGENT_DIR/$BINDIR/dbxcli datasets\n\n")
	writeCLIExamples(&md, "$DBX_AGENT_DIR/$BINDIR/dbxcli", bindings, actionBindings)

	appendSkillMDFooter(&md)
	return md.String(), nil
}

// FindExistingTokenForConfig always returns nil: only token hashes are stored,
// so config.json gets the real token only when a new token is created.
func FindExistingTokenForConfig(_ context.Context, _ *sql.DB, _ int64) *string {
	return nil
}

// ── binary copying ──

// localBinaryCandidates lists where a locally-built/installed binary may live,
// in priority order. Overridable in tests.
var localBinaryCandidates = func(name string) []string {
	var out []string
	if exe, err := os.Executable(); err == nil {
		out = append(out, filepath.Join(filepath.Dir(exe), name))
	}
	out = append(out,
		filepath.Join("/usr/local/bin", name),
		filepath.Join(os.Getenv("HOME"), ".cargo", "bin", name),
	)
	return out
}

// crossTargetDir is the root of cross-compiled builds laid out as
// `<dir>/<triple>/release/<name>`. Empty disables cross copies.
var crossTargetDir = ""

// resolveFile canonicalizes path and reports whether it's a regular file.
func resolveFile(path string) (string, bool) {
	p, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false
	}
	p, err = filepath.Abs(p)
	if err != nil {
		return "", false
	}
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() {
		return "", false
	}
	return p, true
}

// copyExecutable replaces dest with a 0755 copy of src.
func copyExecutable(src, dest string) error {
	_ = os.Remove(dest)
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	_ = os.Chmod(dest, 0o755)
	return nil
}

// copyBinary copies the first locally-available `name` binary into binDir.
// Copies (not symlinks) so the folder works when mounted into containers.
// Silently skips when no source is found.
func copyBinary(binDir, name string) {
	copyLocalInto(binDir, name)
}

// copyLocalInto copies the first resolvable local candidate for name into dir.
func copyLocalInto(dir, name string) {
	for _, cand := range localBinaryCandidates(name) {
		src, ok := resolveFile(cand)
		if !ok {
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			slog.Warn(fmt.Sprintf("failed to create %s: %v", dir, err))
			return
		}
		dest := filepath.Join(dir, name)
		if err := copyExecutable(src, dest); err != nil {
			slog.Warn(fmt.Sprintf("failed to copy %s → %s: %v", src, dest, err))
		}
		return
	}
}

// copyCrossBinary copies a cross-compiled binary (if present) into
// `<dir>/<subdir>/<name>`.
func copyCrossBinary(dir, subdir, triple, name string) {
	if crossTargetDir == "" {
		return
	}
	src, ok := resolveFile(filepath.Join(crossTargetDir, triple, "release", name))
	if !ok {
		return
	}
	targetDir := filepath.Join(dir, subdir)
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		slog.Warn(fmt.Sprintf("failed to create cross-arch bin dir %s: %v", targetDir, err))
		return
	}
	dest := filepath.Join(targetDir, name)
	if err := copyExecutable(src, dest); err != nil {
		slog.Warn(fmt.Sprintf("failed to copy %s → %s: %v", src, dest, err))
	}
}

// copyIntoScripts copies a known binary into `<scriptsDir>/<platform>/<exe>` with 0755.
func copyIntoScripts(scriptsDir, platform, exe, src string) {
	targetDir := filepath.Join(scriptsDir, platform)
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		slog.Warn(fmt.Sprintf("failed to create %s: %v", targetDir, err))
		return
	}
	dest := filepath.Join(targetDir, exe)
	if err := copyExecutable(src, dest); err != nil {
		slog.Warn(fmt.Sprintf("failed to copy %s → %s: %v", src, dest, err))
	}
}
