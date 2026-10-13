# sbx — data gateway daemon (Go)

`sbx` is the data gateway for AI agents: humans configure datasources and datasets, agents consume them through
a uniform HTTP API. This is a Go port of the Rust daemon (`dstudio/crates/dstudiod` + `dstudio-core`). It speaks the
same HTTP API and opens the same SQLite database (`~/.dstudio/dstudio.db`), so it is a drop-in replacement for the
desktop app.

## API surfaces

| Path | Auth | Purpose |
|---|---|---|
| `GET /manage/health` | none | readiness + build provenance |
| `/manage/*` | manage token (`config.toml`) | CRUD for datasources, datasets, agents, actions, approvals, audit, settings, OAuth |
| `/a/{agent_id}/...` | agent token | live dataset API: `datasets`, `{alias}/list`, `get/{id}`, `search`, `schema` |
| `/act/{agent_id}/{alias}` | agent token | actions API (idempotent, approval-gated) |
| `GET /callback` | none | OAuth redirect target |
| `/api-doc/openapi.json`, `/swagger-ui/` | none | OpenAPI spec and UI |

## Running

```bash
make build && ./bin/sbx --port 9820   # data dir defaults to ~/.dstudio
./bin/sbx --host 0.0.0.0 --data-dir /srv/dstudio
./bin/sbx version                     # version, git SHA, build time
./bin/sbx --dump-openapi
./bin/sbx migrate-secrets             # local store → Vault (needs VAULT_ADDR/VAULT_TOKEN)
```

Environment:

| Variable | Effect |
|---|---|
| `DSTUDIO_MANAGE_TOKEN` | overrides the management token from `config.toml` |
| `DSTUDIO_OAUTH_REDIRECT_URL` | overrides `http://127.0.0.1:{port}/callback` |
| `VAULT_ADDR`, `VAULT_TOKEN`, `VAULT_MOUNT`, `VAULT_PREFIX` | store secrets in OpenBao/Vault KV v2 |
| `DSTUDIO_SECRET_STORE=file` | force file-based secrets (`<data_dir>/secrets`) instead of the macOS Keychain |
| `DSTUDIO_LOG` (or `RUST_LOG`) | `debug`, `info`, `warn`, `error` |
| `DBX_CLI_VERSION` | dbxcli version bundled into exported skill packages |

Secrets go to Vault when configured, otherwise the macOS Keychain (via `security`, same service name as the Rust
daemon), otherwise files.

## Docker

```bash
make docker                      # or: docker compose up --build
docker run -p 9820:9820 -v dstudio:/root/.dstudio sbx:0.1.10
```

The image is a static binary (pure-Go SQLite, no cgo) on `distroless/static`.

## Development

```bash
task test      # go test -race ./...
task cover     # coverage profile; fails below 45%
task lint      # gofmt + go vet
task run       # build and run on :9820 (`task run -- --data-dir /tmp/x`)
task           # list all tasks (build:all, docker:build, compose:up, ...)
```

The same core targets are also available through `make` (`build`, `test`, `cover`, `lint`, `docker`).

CI (`.github/workflows/ci.yml`) runs lint, race tests with the 45% coverage gate, cross-builds linux/darwin binaries,
and builds a multi-arch image (pushed to GHCR on `main` and `v*` tags).

## Layout

```
cmd/sbx               entrypoint, CLI, migrate-secrets
internal/server       HTTP routes, middleware, rate limiter, metrics, config.toml
internal/models       domain types (JSON wire shapes)
internal/db           SQLite open + migrations (shared with the Rust daemon)
internal/crud         SQL for datasources/datasets/agents/tokens/bindings
internal/actions      actions, predefined catalog, approvals, idempotency
internal/audit        batched audit-log writer and pruner
internal/cache        per-dataset SQLite caches
internal/connector    Connector interface, errors, in-memory filters
internal/connectors/* gmail, gcal, gdrive, jira, github, linear, postgres, rest (+OpenAPI), imap, fileupload
internal/factory      datasource → connector
internal/agentdir     agent config.json / SKILL.md generation and skill packages
internal/oauth        OAuth2 + PKCE flows and token storage
internal/secretstore  Vault, Keychain, file and in-memory secret stores
internal/{pii,synthetic,ai,clidist,exportdata,capabilities,entitycatalog,util,version}
```
