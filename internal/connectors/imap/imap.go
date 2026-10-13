// Package imap implements the IMAP/SMTP email connector: reading a mailbox
// over IMAPS, flag/move actions, and sending mail over SMTP.
package imap

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	goimap "github.com/emersion/go-imap"

	"github.com/ipedrazas/sbx/internal/connector"
	"github.com/ipedrazas/sbx/internal/models"
	"github.com/ipedrazas/sbx/internal/util"
)

// DatasourceConfig mirrors ImapDatasourceConfig (stored in datasources.config).
type DatasourceConfig struct {
	ImapHost    string  `json:"imap_host"`
	ImapPort    uint16  `json:"imap_port"`
	SMTPHost    *string `json:"smtp_host"`
	SMTPPort    uint16  `json:"smtp_port"`
	Username    string  `json:"username"`
	PasswordRef string  `json:"password_ref"`
}

// UnmarshalJSON applies serde defaults (imap_port 993, smtp_port 587) and
// requires imap_host, username and password_ref.
func (c *DatasourceConfig) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	for _, k := range []string{"imap_host", "username", "password_ref"} {
		if _, ok := raw[k]; !ok {
			return fmt.Errorf("missing field `%s`", k)
		}
	}
	type plain DatasourceConfig
	p := plain{ImapPort: 993, SMTPPort: 587}
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*c = DatasourceConfig(p)
	return nil
}

// ParseDatasourceConfig decodes a datasources.config map.
func ParseDatasourceConfig(m map[string]any) (DatasourceConfig, error) {
	var c DatasourceConfig
	b, err := json.Marshal(m)
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(b, &c)
	return c, err
}

// DefaultSMTPHost derives the SMTP host when smtp_host is unset
// ("imap." → "smtp." in the IMAP host), as the connector factory does.
func (c DatasourceConfig) DefaultSMTPHost() string {
	if c.SMTPHost != nil {
		return *c.SMTPHost
	}
	return strings.ReplaceAll(c.ImapHost, "imap.", "smtp.")
}

// Config mirrors ImapConnectorConfig (resolved password, chosen mailbox).
type Config struct {
	ImapHost string
	ImapPort uint16
	SMTPHost string
	SMTPPort uint16
	Username string
	Password string
	Mailbox  string
}

type Connector struct {
	connector.Base
	datasourceID int64
	config       Config
}

var _ connector.Connector = (*Connector)(nil)

func New(datasourceID int64, cfg Config) *Connector {
	return &Connector{datasourceID: datasourceID, config: cfg}
}

func (c *Connector) Introspect(context.Context) (*connector.Schema, error) {
	f := func(name string, t connector.FieldType, desc string) connector.SchemaField {
		return connector.SchemaField{Name: name, FieldType: t, Description: &desc}
	}
	return &connector.Schema{Fields: []connector.SchemaField{
		f("id", connector.FieldString, "Message UID"),
		f("message_id", connector.FieldString, "RFC 2822 Message-ID"),
		f("from", connector.FieldString, "Sender address"),
		f("to", connector.FieldString, "Recipient address(es)"),
		f("subject", connector.FieldString, "Email subject line"),
		f("date", connector.FieldDateTime, "Date the email was sent"),
		f("snippet", connector.FieldString, "Body preview"),
		f("flags", connector.FieldJSON, "IMAP flags"),
	}}, nil
}

func (c *Connector) Query(ctx context.Context, params *connector.QueryParams) (*models.Envelope, error) {
	s, err := dialIMAP(ctx, c.config)
	if err != nil {
		return nil, err
	}
	defer s.Logout()
	if err := s.Select(c.config.Mailbox); err != nil {
		return nil, connector.Query("SELECT %s: %v", c.config.Mailbox, err)
	}

	if params.ItemID != nil {
		msgs, err := s.UidFetch(*params.ItemID)
		if err != nil {
			return nil, connector.Query("%v", err)
		}
		items := make([]map[string]any, len(msgs))
		for i, m := range msgs {
			items[i] = fetchToMap(m)
		}
		return &models.Envelope{Items: items, Meta: models.EnvelopeMeta{Total: int64(len(items)), Limit: 1, Offset: 0}}, nil
	}

	query, inMem := buildIMAPSearch(params.Filters, params.Search)
	uids, err := s.UidSearch(query)
	if err != nil {
		return nil, connector.Query("SEARCH: %v", err)
	}
	slices.SortFunc(uids, func(a, b uint32) int { return int(int64(b) - int64(a)) })

	total := int64(len(uids))
	start := max(params.Offset, 0)
	end := min(start+max(params.Limit, 0), total)
	items := []map[string]any{}
	if start < total {
		page := uids[start:end]
		if len(page) > 0 {
			parts := make([]string, len(page))
			for i, u := range page {
				parts[i] = strconv.FormatUint(uint64(u), 10)
			}
			msgs, err := s.UidFetch(strings.Join(parts, ","))
			if err != nil {
				return nil, connector.Query("%v", err)
			}
			for _, m := range msgs {
				items = append(items, fetchToMap(m))
			}
		}
	}
	items = connector.ApplyFiltersInMemory(items, inMem)
	return &models.Envelope{Items: items, Meta: models.EnvelopeMeta{Total: total, Limit: params.Limit, Offset: params.Offset}}, nil
}

func (c *Connector) TestConnection(ctx context.Context) error {
	s, err := dialIMAP(ctx, c.config)
	if err != nil {
		return err
	}
	defer s.Logout()
	if err := s.Select(c.config.Mailbox); err != nil {
		return connector.Connection("SELECT %s: %v", c.config.Mailbox, err)
	}
	return nil
}

func (c *Connector) InvokeAction(ctx context.Context, actionType string, params map[string]any) (any, error) {
	switch actionType {
	case "send_email":
		req, err := parseSendEmailParams(params)
		if err != nil {
			return nil, connector.Query("invalid send_email params: %v", err)
		}
		return sendEmailSMTP(ctx, c.config, req)
	case "mark_read", "mark_unread", "flag_message", "unflag_message", "delete_message":
		return imapStoreFlags(ctx, c.config, actionType, params)
	case "move_message":
		return imapMoveMessage(ctx, c.config, params)
	}
	return nil, connector.Unsupported("IMAP connector does not support action '%s'", actionType)
}

func (c *Connector) SimulateAction(_ context.Context, actionType string, params map[string]any) (any, error) {
	if actionType != "send_email" {
		return nil, connector.Unsupported("IMAP connector does not simulate action '%s'", actionType)
	}
	if _, err := parseSendEmailParams(params); err != nil {
		return nil, connector.Query("invalid send_email params: %v", err)
	}
	suffix := util.HashCanonicalJSON(params)
	return map[string]any{"status": "sent", "message_id": suffix[:16] + "@sim.imap"}, nil
}

func (c *Connector) GenerateSyntheticRows(_ []models.DatasetField, count int64) ([]map[string]any, bool) {
	return generateIMAPRows(count), true
}

// buildIMAPSearch pushes from/to/subject filters and the search term into an
// IMAP SEARCH string; everything else is returned for in-memory filtering.
func buildIMAPSearch(filters []models.DatasetFilter, search *string) (string, []models.DatasetFilter) {
	var parts []string
	var inMem []models.DatasetFilter
	for _, f := range filters {
		v := util.ValueAsString(f.Value)
		switch {
		case f.FieldName == "from" && (f.Operator == models.OpEquals || f.Operator == models.OpContains):
			parts = append(parts, fmt.Sprintf("FROM \"%s\"", v))
		case f.FieldName == "to" && (f.Operator == models.OpEquals || f.Operator == models.OpContains):
			parts = append(parts, fmt.Sprintf("TO \"%s\"", v))
		case f.FieldName == "subject" && f.Operator == models.OpContains:
			parts = append(parts, fmt.Sprintf("SUBJECT \"%s\"", v))
		default:
			inMem = append(inMem, f)
		}
	}
	if search != nil {
		parts = append(parts, fmt.Sprintf("TEXT \"%s\"", *search))
	}
	if len(parts) == 0 {
		return "ALL", inMem
	}
	return strings.Join(parts, " "), inMem
}

func fetchToMap(m *goimap.Message) map[string]any {
	var from, to, subject, date, messageID string
	if env := m.Envelope; env != nil {
		from = formatAddresses(env.From)
		to = formatAddresses(env.To)
		subject = env.Subject
		if !env.Date.IsZero() {
			date = env.Date.Format("Mon, 02 Jan 2006 15:04:05 -0700")
		}
		messageID = env.MessageId
	}
	flags := make([]any, len(m.Flags))
	for i, f := range m.Flags {
		flags[i] = f
	}
	return map[string]any{
		"id":         strconv.FormatUint(uint64(m.Uid), 10),
		"message_id": messageID,
		"from":       from,
		"to":         to,
		"subject":    subject,
		"date":       date,
		"snippet":    "",
		"flags":      flags,
	}
}

func formatAddresses(addrs []*goimap.Address) string {
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		email := a.MailboxName
		if a.HostName != "" {
			email = a.MailboxName + "@" + a.HostName
		}
		if a.PersonalName != "" {
			out = append(out, fmt.Sprintf("%s <%s>", a.PersonalName, email))
		} else {
			out = append(out, email)
		}
	}
	return strings.Join(out, ", ")
}

// ── IMAP flag operations ───────────────────────────────────────

var flagOps = map[string]struct {
	item goimap.StoreItem
	flag string
}{
	"mark_read":      {goimap.FormatFlagsOp(goimap.AddFlags, false), goimap.SeenFlag},
	"mark_unread":    {goimap.FormatFlagsOp(goimap.RemoveFlags, false), goimap.SeenFlag},
	"flag_message":   {goimap.FormatFlagsOp(goimap.AddFlags, false), goimap.FlaggedFlag},
	"unflag_message": {goimap.FormatFlagsOp(goimap.RemoveFlags, false), goimap.FlaggedFlag},
	"delete_message": {goimap.FormatFlagsOp(goimap.AddFlags, false), goimap.DeletedFlag},
}

func strParam(params map[string]any, key string) (string, error) {
	s, ok := params[key].(string)
	if !ok {
		return "", connector.Query("missing '%s' parameter", key)
	}
	return s, nil
}

func imapStoreFlags(ctx context.Context, cfg Config, actionType string, params map[string]any) (any, error) {
	uid, err := strParam(params, "uid")
	if err != nil {
		return nil, err
	}
	op, ok := flagOps[actionType]
	if !ok {
		return nil, connector.Unsupported("%s", actionType)
	}
	s, err := dialIMAP(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer s.Logout()
	if err := s.Select(cfg.Mailbox); err != nil {
		return nil, connector.Query("SELECT: %v", err)
	}
	if err := s.UidStore(uid, op.item, op.flag); err != nil {
		return nil, connector.Query("STORE: %v", err)
	}
	if actionType == "delete_message" {
		if err := s.Expunge(); err != nil {
			return nil, connector.Query("EXPUNGE: %v", err)
		}
	}
	return map[string]any{"success": true}, nil
}

func imapMoveMessage(ctx context.Context, cfg Config, params map[string]any) (any, error) {
	uid, err := strParam(params, "uid")
	if err != nil {
		return nil, err
	}
	dest, err := strParam(params, "destination")
	if err != nil {
		return nil, err
	}
	s, err := dialIMAP(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer s.Logout()
	if err := s.Select(cfg.Mailbox); err != nil {
		return nil, connector.Query("SELECT: %v", err)
	}
	if err := s.UidCopy(uid, dest); err != nil {
		return nil, connector.Query("COPY: %v", err)
	}
	if err := s.UidStore(uid, goimap.FormatFlagsOp(goimap.AddFlags, false), goimap.DeletedFlag); err != nil {
		return nil, connector.Query("STORE: %v", err)
	}
	if err := s.Expunge(); err != nil {
		return nil, connector.Query("EXPUNGE: %v", err)
	}
	return map[string]any{"success": true}, nil
}

// ── send_email params ──────────────────────────────────────────

type sendEmailParams struct {
	To      string
	Subject string
	Body    string
	Cc      *string
	Bcc     *string
}

// parseSendEmailParams mirrors serde's decoding of SendEmailParams.
func parseSendEmailParams(params map[string]any) (*sendEmailParams, error) {
	req := &sendEmailParams{}
	for _, f := range []struct {
		key string
		dst *string
	}{{"to", &req.To}, {"subject", &req.Subject}, {"body", &req.Body}} {
		v, ok := params[f.key]
		if !ok {
			return nil, fmt.Errorf("missing field `%s`", f.key)
		}
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("invalid type: %s, expected a string", describe(v))
		}
		*f.dst = s
	}
	for _, f := range []struct {
		key string
		dst **string
	}{{"cc", &req.Cc}, {"bcc", &req.Bcc}} {
		switch v := params[f.key].(type) {
		case nil:
		case string:
			*f.dst = &v
		default:
			return nil, fmt.Errorf("invalid type: %s, expected a string", describe(v))
		}
	}
	return req, nil
}

func describe(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case bool:
		return fmt.Sprintf("boolean `%v`", t)
	case json.Number, float64, int, int64:
		return fmt.Sprintf("number `%s`", util.ValueAsString(t))
	case []any:
		return "sequence"
	case map[string]any:
		return "map"
	}
	return "value"
}
