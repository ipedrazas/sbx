package db

// migrations is the ordered list of schema migrations, copied verbatim from
// the Rust daemon so both share one database. IDs are 1-based positions; an
// empty entry is a reserved no-op slot. Statements are split on ';'.
var migrations = []string{
	`
    CREATE TABLE IF NOT EXISTS _migrations (
        id INTEGER PRIMARY KEY,
        applied_at TEXT NOT NULL DEFAULT (datetime('now'))
    );

    CREATE TABLE IF NOT EXISTS datasources (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        name TEXT NOT NULL,
        ds_type TEXT NOT NULL,
        config TEXT NOT NULL DEFAULT '{}',
        created_at TEXT NOT NULL DEFAULT (datetime('now')),
        updated_at TEXT NOT NULL DEFAULT (datetime('now'))
    );

    CREATE TABLE IF NOT EXISTS datasets (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        name TEXT NOT NULL,
        datasource_id INTEGER NOT NULL REFERENCES datasources(id),
        mode TEXT NOT NULL DEFAULT 'pull_through',
        cache_ttl_seconds INTEGER,
        created_at TEXT NOT NULL DEFAULT (datetime('now')),
        updated_at TEXT NOT NULL DEFAULT (datetime('now'))
    );

    CREATE TABLE IF NOT EXISTS dataset_fields (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        dataset_id INTEGER NOT NULL REFERENCES datasets(id) ON DELETE CASCADE,
        source_name TEXT NOT NULL,
        display_name TEXT NOT NULL,
        field_type TEXT NOT NULL DEFAULT 'string',
        description TEXT,
        visible INTEGER NOT NULL DEFAULT 1
    );

    CREATE TABLE IF NOT EXISTS dataset_filters (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        dataset_id INTEGER NOT NULL REFERENCES datasets(id) ON DELETE CASCADE,
        field_name TEXT NOT NULL,
        operator TEXT NOT NULL,
        value TEXT NOT NULL,
        logic TEXT NOT NULL DEFAULT 'and',
        group_index INTEGER NOT NULL DEFAULT 0
    );

    CREATE TABLE IF NOT EXISTS bearer_tokens (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        dataset_id INTEGER NOT NULL REFERENCES datasets(id) ON DELETE CASCADE,
        token_hash TEXT NOT NULL,
        label TEXT NOT NULL DEFAULT '',
        created_at TEXT NOT NULL DEFAULT (datetime('now'))
    );
    `,
	`
    ALTER TABLE datasets ADD COLUMN source_config TEXT NOT NULL DEFAULT '{}'
    `,
	``,
	`
    ALTER TABLE datasets ADD COLUMN enabled INTEGER NOT NULL DEFAULT 1
    `,
	`
    CREATE TABLE IF NOT EXISTS oauth_connections (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        name TEXT NOT NULL,
        provider TEXT NOT NULL,
        client_id TEXT NOT NULL,
        client_secret TEXT,
        created_at TEXT NOT NULL DEFAULT (datetime('now')),
        updated_at TEXT NOT NULL DEFAULT (datetime('now'))
    );
    ALTER TABLE datasources ADD COLUMN connection_id INTEGER REFERENCES oauth_connections(id)
    `,
	`
    CREATE TABLE IF NOT EXISTS agents (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        name TEXT NOT NULL,
        description TEXT,
        created_at TEXT NOT NULL DEFAULT (datetime('now')),
        updated_at TEXT NOT NULL DEFAULT (datetime('now'))
    );
    CREATE TABLE IF NOT EXISTS agent_datasets (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        agent_id INTEGER NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
        dataset_id INTEGER NOT NULL REFERENCES datasets(id) ON DELETE CASCADE,
        alias TEXT NOT NULL,
        UNIQUE(agent_id, dataset_id),
        UNIQUE(agent_id, alias)
    );
    CREATE TABLE IF NOT EXISTS agent_tokens (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        agent_id INTEGER NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
        token_hash TEXT NOT NULL,
        label TEXT NOT NULL DEFAULT '',
        created_at TEXT NOT NULL DEFAULT (datetime('now'))
    );
    DROP TABLE IF EXISTS bearer_tokens
    `,
	`
    ALTER TABLE datasets ADD COLUMN description TEXT;
    ALTER TABLE datasets ADD COLUMN data_kind TEXT
    `,
	`
    CREATE TABLE IF NOT EXISTS audit_log (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        ts TEXT NOT NULL DEFAULT (datetime('now')),
        agent_id INTEGER,
        dataset_id INTEGER,
        alias TEXT,
        endpoint TEXT NOT NULL,
        status INTEGER NOT NULL,
        bytes_out INTEGER NOT NULL DEFAULT 0,
        latency_ms INTEGER NOT NULL DEFAULT 0
    );
    CREATE INDEX IF NOT EXISTS idx_audit_log_ts ON audit_log(ts);
    CREATE INDEX IF NOT EXISTS idx_audit_log_agent_ts ON audit_log(agent_id, ts);
    CREATE INDEX IF NOT EXISTS idx_audit_log_dataset_ts ON audit_log(dataset_id, ts)
    `,
	`
    ALTER TABLE datasets ADD COLUMN max_rows_per_query INTEGER
    `,
	`
    ALTER TABLE agent_datasets ADD COLUMN requests_per_minute INTEGER
    `,
	`
    ALTER TABLE dataset_fields ADD COLUMN searchable INTEGER NOT NULL DEFAULT 1
    `,
	`
    CREATE TABLE IF NOT EXISTS actions (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        datasource_id INTEGER NOT NULL REFERENCES datasources(id) ON DELETE CASCADE,
        name TEXT NOT NULL,
        action_type TEXT NOT NULL,
        description TEXT,
        params_schema TEXT NOT NULL DEFAULT '{}',
        result_schema TEXT NOT NULL DEFAULT '{}',
        state TEXT NOT NULL DEFAULT 'enabled',
        policy_kind TEXT NOT NULL DEFAULT 'auto',
        threshold_config TEXT NOT NULL DEFAULT '{}',
        approval_ttl_seconds INTEGER DEFAULT 86400,
        created_at TEXT NOT NULL DEFAULT (datetime('now')),
        updated_at TEXT NOT NULL DEFAULT (datetime('now')),
        UNIQUE(datasource_id, action_type)
    );
    CREATE TABLE IF NOT EXISTS agent_actions (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        agent_id INTEGER NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
        action_id INTEGER NOT NULL REFERENCES actions(id) ON DELETE CASCADE,
        alias TEXT NOT NULL,
        requests_per_minute INTEGER,
        UNIQUE(agent_id, action_id),
        UNIQUE(agent_id, alias)
    );
    ALTER TABLE agent_tokens ADD COLUMN force_simulate INTEGER NOT NULL DEFAULT 0;
    CREATE TABLE IF NOT EXISTS action_approvals (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        agent_id INTEGER NOT NULL,
        action_id INTEGER NOT NULL,
        token_id INTEGER,
        idempotency_key TEXT,
        params TEXT NOT NULL,
        params_hash TEXT NOT NULL,
        simulated INTEGER NOT NULL DEFAULT 0,
        state TEXT NOT NULL DEFAULT 'pending',
        result TEXT,
        created_at TEXT NOT NULL DEFAULT (datetime('now')),
        expires_at TEXT,
        resolved_at TEXT,
        resolved_by TEXT
    );
    CREATE INDEX IF NOT EXISTS idx_action_approvals_state ON action_approvals(state);
    CREATE INDEX IF NOT EXISTS idx_action_approvals_expires ON action_approvals(expires_at) WHERE state = 'pending';
    CREATE INDEX IF NOT EXISTS idx_action_approvals_agent_ts ON action_approvals(agent_id, created_at);
    CREATE TABLE IF NOT EXISTS action_idempotency (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        token_id INTEGER NOT NULL REFERENCES agent_tokens(id) ON DELETE CASCADE,
        action_id INTEGER NOT NULL REFERENCES actions(id) ON DELETE CASCADE,
        key TEXT NOT NULL,
        simulated INTEGER NOT NULL DEFAULT 0,
        params_hash TEXT NOT NULL,
        status INTEGER NOT NULL,
        result TEXT NOT NULL,
        created_at TEXT NOT NULL DEFAULT (datetime('now')),
        expires_at TEXT NOT NULL,
        UNIQUE(token_id, action_id, key, simulated)
    );
    CREATE INDEX IF NOT EXISTS idx_action_idempotency_expires ON action_idempotency(expires_at);
    ALTER TABLE audit_log ADD COLUMN action_id INTEGER;
    ALTER TABLE audit_log ADD COLUMN approval_id INTEGER;
    ALTER TABLE audit_log ADD COLUMN idempotency_key TEXT;
    ALTER TABLE audit_log ADD COLUMN params_hash TEXT;
    ALTER TABLE audit_log ADD COLUMN simulated INTEGER NOT NULL DEFAULT 0;
    ALTER TABLE audit_log ADD COLUMN approval_state TEXT;
    CREATE INDEX IF NOT EXISTS idx_audit_log_action_ts ON audit_log(action_id, ts)
    `,
	`
    ALTER TABLE agent_datasets ADD COLUMN rate_limit_count INTEGER;
    ALTER TABLE agent_datasets ADD COLUMN rate_limit_period_seconds INTEGER;
    UPDATE agent_datasets
       SET rate_limit_count = requests_per_minute,
           rate_limit_period_seconds = 60
     WHERE requests_per_minute IS NOT NULL;
    ALTER TABLE agent_actions ADD COLUMN rate_limit_count INTEGER;
    ALTER TABLE agent_actions ADD COLUMN rate_limit_period_seconds INTEGER;
    UPDATE agent_actions
       SET rate_limit_count = requests_per_minute,
           rate_limit_period_seconds = 60
     WHERE requests_per_minute IS NOT NULL
    `,
	`
    CREATE TABLE IF NOT EXISTS agent_skill_installations (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        agent_id INTEGER NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
        folder_path TEXT NOT NULL,
        installed_at TEXT NOT NULL DEFAULT (datetime('now')),
        updated_at TEXT NOT NULL DEFAULT (datetime('now')),
        UNIQUE(agent_id, folder_path)
    );
    CREATE INDEX IF NOT EXISTS idx_skill_installations_agent ON agent_skill_installations(agent_id)
    `,
	`
    ALTER TABLE agent_actions ADD COLUMN template TEXT
    `,
	`
    ALTER TABLE oauth_connections ADD COLUMN scopes TEXT NOT NULL DEFAULT '[]'
    `,
	`
    ALTER TABLE agents ADD COLUMN agent_type TEXT;
    ALTER TABLE agents ADD COLUMN owner TEXT;
    ALTER TABLE agents ADD COLUMN status TEXT;
    ALTER TABLE agents ADD COLUMN mission TEXT
    `,
	`
    ALTER TABLE dataset_fields ADD COLUMN pii_class TEXT;
    ALTER TABLE dataset_fields ADD COLUMN pii_policy TEXT NOT NULL DEFAULT 'none';
    ALTER TABLE dataset_fields ADD COLUMN credential_detected INTEGER NOT NULL DEFAULT 0;
    ALTER TABLE datasets ADD COLUMN pii_scan_at TEXT;
    ALTER TABLE datasets ADD COLUMN require_pii_review INTEGER NOT NULL DEFAULT 0
    `,
	`
    ALTER TABLE actions ADD COLUMN risk_level TEXT NOT NULL DEFAULT 'medium';
    ALTER TABLE actions ADD COLUMN required_scopes TEXT NOT NULL DEFAULT '[]';
    ALTER TABLE actions ADD COLUMN predefined INTEGER NOT NULL DEFAULT 0
    `,
	`
    ALTER TABLE datasets ADD COLUMN snapshot_at TEXT;
    ALTER TABLE datasets ADD COLUMN synthetic_row_count INTEGER DEFAULT 500
    `,
	`
    ALTER TABLE agent_actions ADD COLUMN pinned_params TEXT
    `,
	`
    ALTER TABLE datasources ADD COLUMN read_only INTEGER NOT NULL DEFAULT 0
    `,
	`
    ALTER TABLE datasets ADD COLUMN max_page_bytes INTEGER
    `,
	`
    ALTER TABLE dataset_fields ADD COLUMN sortable INTEGER NOT NULL DEFAULT 1
    `,
}
