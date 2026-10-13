// Package models holds the domain types shared across the daemon. JSON tags
// mirror the Rust serde representation so the Go daemon is wire-compatible
// with the existing desktop app and agents.
package models

// DatasourceType identifies the connector backing a datasource.
type DatasourceType string

const (
	DatasourceGmail          DatasourceType = "gmail"
	DatasourceGoogleCalendar DatasourceType = "google_calendar"
	DatasourceGoogleDrive    DatasourceType = "google_drive"
	DatasourcePostgres       DatasourceType = "postgres"
	DatasourceJira           DatasourceType = "jira"
	DatasourceLinear         DatasourceType = "linear"
	DatasourceRest           DatasourceType = "rest"
	DatasourceGithub         DatasourceType = "github"
	DatasourceHubspot        DatasourceType = "hubspot"
	DatasourceImap           DatasourceType = "imap"
	DatasourceFileUpload     DatasourceType = "file_upload"
)

// AllDatasourceTypes lists every known datasource type.
var AllDatasourceTypes = []DatasourceType{
	DatasourceGmail, DatasourceGoogleCalendar, DatasourceGoogleDrive, DatasourcePostgres,
	DatasourceJira, DatasourceLinear, DatasourceRest, DatasourceGithub, DatasourceHubspot,
	DatasourceImap, DatasourceFileUpload,
}

// Valid reports whether t is a known datasource type.
func (t DatasourceType) Valid() bool {
	for _, k := range AllDatasourceTypes {
		if k == t {
			return true
		}
	}
	return false
}

// JSONMap is a decoded JSON object (serde_json::Value used as an object).
type JSONMap = map[string]any

type Datasource struct {
	ID     int64          `json:"id"`
	Name   string         `json:"name"`
	DsType DatasourceType `json:"ds_type"`
	Config JSONMap        `json:"config"`
	// Optional reference to a shared OAuth connection.
	ConnectionID *int64 `json:"connection_id"`
	// When true, all write actions on this datasource are blocked.
	ReadOnly  bool   `json:"read_only"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type OAuthConnection struct {
	ID           int64    `json:"id"`
	Name         string   `json:"name"`
	Provider     string   `json:"provider"`
	ClientID     string   `json:"client_id"`
	ClientSecret *string  `json:"client_secret,omitempty"`
	Scopes       []string `json:"scopes"`
	CreatedAt    string   `json:"created_at"`
	UpdatedAt    string   `json:"updated_at"`
}

type DatasetMode string

const (
	ModePullThrough DatasetMode = "pull_through"
	ModeCached      DatasetMode = "cached"
	ModeMirror      DatasetMode = "mirror"
	ModeSynthetic   DatasetMode = "synthetic"
)

// Valid reports whether m is a known dataset mode.
func (m DatasetMode) Valid() bool {
	switch m {
	case ModePullThrough, ModeCached, ModeMirror, ModeSynthetic:
		return true
	}
	return false
}

type Dataset struct {
	ID                int64       `json:"id"`
	Name              string      `json:"name"`
	DatasourceID      int64       `json:"datasource_id"`
	Mode              DatasetMode `json:"mode"`
	CacheTTLSeconds   *int64      `json:"cache_ttl_seconds"`
	SourceConfig      JSONMap     `json:"source_config"`
	Enabled           bool        `json:"enabled"`
	Description       *string     `json:"description,omitempty"`
	DataKind          *string     `json:"data_kind,omitempty"`
	MaxRowsPerQuery   *int64      `json:"max_rows_per_query,omitempty"`
	PiiScanAt         *string     `json:"pii_scan_at,omitempty"`
	RequirePiiReview  bool        `json:"require_pii_review"`
	SnapshotAt        *string     `json:"snapshot_at,omitempty"`
	SyntheticRowCount *int64      `json:"synthetic_row_count,omitempty"`
	MaxPageBytes      *int64      `json:"max_page_bytes,omitempty"`
	CreatedAt         string      `json:"created_at"`
	UpdatedAt         string      `json:"updated_at"`
}

type DatasetField struct {
	ID          int64   `json:"id"`
	DatasetID   int64   `json:"dataset_id"`
	SourceName  string  `json:"source_name"`
	DisplayName string  `json:"display_name"`
	FieldType   string  `json:"field_type"`
	Description *string `json:"description"`
	Visible     bool    `json:"visible"`
	// When false, values are excluded from /search matching. Defaults to true
	// when absent from JSON input — see NewDatasetField / UnmarshalJSON.
	Searchable bool `json:"searchable"`
	// When false, agents cannot sort by this field. Defaults to true.
	Sortable           bool    `json:"sortable"`
	PiiClass           *string `json:"pii_class,omitempty"`
	PiiPolicy          string  `json:"pii_policy"`
	CredentialDetected bool    `json:"credential_detected"`
}

// NewDatasetField returns a field with the same defaults as the Rust
// `DatasetField::default()`.
func NewDatasetField() DatasetField {
	return DatasetField{
		FieldType:  "string",
		Visible:    true,
		Searchable: true,
		Sortable:   true,
		PiiPolicy:  "none",
	}
}

type FilterOperator string

const (
	OpEquals      FilterOperator = "equals"
	OpNotEquals   FilterOperator = "not_equals"
	OpContains    FilterOperator = "contains"
	OpNotContains FilterOperator = "not_contains"
	OpGreaterThan FilterOperator = "greater_than"
	OpLessThan    FilterOperator = "less_than"
	OpIn          FilterOperator = "in"
	OpNotIn       FilterOperator = "not_in"
)

// Valid reports whether o is a known filter operator.
func (o FilterOperator) Valid() bool {
	switch o {
	case OpEquals, OpNotEquals, OpContains, OpNotContains, OpGreaterThan, OpLessThan, OpIn, OpNotIn:
		return true
	}
	return false
}

type FilterLogic string

const (
	LogicAnd FilterLogic = "and"
	LogicOr  FilterLogic = "or"
)

type DatasetFilter struct {
	ID         int64          `json:"id"`
	DatasetID  int64          `json:"dataset_id"`
	FieldName  string         `json:"field_name"`
	Operator   FilterOperator `json:"operator"`
	Value      any            `json:"value"`
	Logic      FilterLogic    `json:"logic"`
	GroupIndex int32          `json:"group_index"`
}

type Agent struct {
	ID          int64   `json:"id"`
	Name        string  `json:"name"`
	Description *string `json:"description,omitempty"`
	AgentType   *string `json:"agent_type,omitempty"`
	Owner       *string `json:"owner,omitempty"`
	Status      *string `json:"status,omitempty"`
	Mission     *string `json:"mission,omitempty"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
}

type AgentDataset struct {
	ID                     int64  `json:"id"`
	AgentID                int64  `json:"agent_id"`
	DatasetID              int64  `json:"dataset_id"`
	Alias                  string `json:"alias"`
	RateLimitCount         *int64 `json:"rate_limit_count,omitempty"`
	RateLimitPeriodSeconds *int64 `json:"rate_limit_period_seconds,omitempty"`
}

type AgentSkillInstallation struct {
	ID          int64  `json:"id"`
	AgentID     int64  `json:"agent_id"`
	FolderPath  string `json:"folder_path"`
	InstalledAt string `json:"installed_at"`
	UpdatedAt   string `json:"updated_at"`
}

type AgentToken struct {
	ID            int64  `json:"id"`
	AgentID       int64  `json:"agent_id"`
	TokenHash     string `json:"token_hash"`
	Label         string `json:"label"`
	ForceSimulate bool   `json:"force_simulate"`
	CreatedAt     string `json:"created_at"`
}

type RiskLevel string

const (
	RiskLow      RiskLevel = "low"
	RiskMedium   RiskLevel = "medium"
	RiskHigh     RiskLevel = "high"
	RiskCritical RiskLevel = "critical"
)

type ActionState string

const (
	ActionEnabled  ActionState = "enabled"
	ActionDisabled ActionState = "disabled"
	ActionPaused   ActionState = "paused"
)

type PolicyKind string

const (
	PolicyAuto          PolicyKind = "auto"
	PolicyAlwaysApprove PolicyKind = "always_approve"
	PolicyThreshold     PolicyKind = "threshold"
)

type ApprovalState string

const (
	ApprovalPending  ApprovalState = "pending"
	ApprovalApproved ApprovalState = "approved"
	ApprovalDenied   ApprovalState = "denied"
	ApprovalExpired  ApprovalState = "expired"
	ApprovalExecuted ApprovalState = "executed"
	ApprovalFailed   ApprovalState = "failed"
)

type Action struct {
	ID                 int64       `json:"id"`
	DatasourceID       int64       `json:"datasource_id"`
	Name               string      `json:"name"`
	ActionType         string      `json:"action_type"`
	Description        *string     `json:"description,omitempty"`
	ParamsSchema       JSONMap     `json:"params_schema"`
	ResultSchema       JSONMap     `json:"result_schema"`
	State              ActionState `json:"state"`
	PolicyKind         PolicyKind  `json:"policy_kind"`
	ThresholdConfig    JSONMap     `json:"threshold_config"`
	ApprovalTTLSeconds *int64      `json:"approval_ttl_seconds,omitempty"`
	RiskLevel          RiskLevel   `json:"risk_level"`
	RequiredScopes     []string    `json:"required_scopes"`
	Predefined         bool        `json:"predefined"`
	CreatedAt          string      `json:"created_at"`
	UpdatedAt          string      `json:"updated_at"`
}

type AgentAction struct {
	ID                     int64   `json:"id"`
	AgentID                int64   `json:"agent_id"`
	ActionID               int64   `json:"action_id"`
	Alias                  string  `json:"alias"`
	RateLimitCount         *int64  `json:"rate_limit_count,omitempty"`
	RateLimitPeriodSeconds *int64  `json:"rate_limit_period_seconds,omitempty"`
	Template               *string `json:"template,omitempty"`
	PinnedParams           any     `json:"pinned_params,omitempty"`
}

type ActionApproval struct {
	ID             int64         `json:"id"`
	AgentID        int64         `json:"agent_id"`
	ActionID       int64         `json:"action_id"`
	TokenID        *int64        `json:"token_id,omitempty"`
	IdempotencyKey *string       `json:"idempotency_key,omitempty"`
	Params         any           `json:"params"`
	ParamsHash     string        `json:"params_hash"`
	Simulated      bool          `json:"simulated"`
	State          ApprovalState `json:"state"`
	Result         any           `json:"result,omitempty"`
	CreatedAt      string        `json:"created_at"`
	ExpiresAt      *string       `json:"expires_at,omitempty"`
	ResolvedAt     *string       `json:"resolved_at,omitempty"`
	ResolvedBy     *string       `json:"resolved_by,omitempty"`
}

type ActionIdempotency struct {
	ID         int64  `json:"id"`
	TokenID    int64  `json:"token_id"`
	ActionID   int64  `json:"action_id"`
	Key        string `json:"key"`
	Simulated  bool   `json:"simulated"`
	ParamsHash string `json:"params_hash"`
	Status     int64  `json:"status"`
	Result     any    `json:"result"`
	CreatedAt  string `json:"created_at"`
	ExpiresAt  string `json:"expires_at"`
}

// SortSpec requests ordering by a single source_name field.
type SortSpec struct {
	Field      string
	Descending bool
}

// Envelope is the paginated response shape returned by connectors and the live API.
type Envelope struct {
	Items []map[string]any `json:"items"`
	Meta  EnvelopeMeta     `json:"meta"`
}

type EnvelopeMeta struct {
	Total         int64   `json:"total"`
	Limit         int64   `json:"limit"`
	Offset        int64   `json:"offset"`
	ResponseBytes *int64  `json:"response_bytes,omitempty"`
	SortedBy      *string `json:"sorted_by,omitempty"`
	SortedDir     *string `json:"sorted_dir,omitempty"`
}

// Ptr returns a pointer to v. Handy for optional fields.
func Ptr[T any](v T) *T { return &v }
