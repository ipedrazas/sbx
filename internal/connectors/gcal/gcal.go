// Package gcal implements the Google Calendar connector (read-only events).
package gcal

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/ipedrazas/sbx/internal/connector"
	"github.com/ipedrazas/sbx/internal/models"
	"github.com/ipedrazas/sbx/internal/oauth"
	"github.com/ipedrazas/sbx/internal/util"
)

// APIBase is the real Calendar API base URL.
const APIBase = "https://www.googleapis.com/calendar/v3"

// now is overridable in tests.
var now = func() int64 { return time.Now().Unix() }

type Connector struct {
	connector.Base
	datasourceID int64
	credentials  oauth.Credentials
	calendarID   string
	// BaseURL defaults to APIBase; overridable for tests.
	BaseURL string
	// TokenURL defaults to oauth.GoogleTokenURL.
	TokenURL string
	Client   *http.Client
}

// New builds a connector; calendarID defaults to "primary" when nil.
func New(datasourceID int64, creds oauth.Credentials, calendarID *string) *Connector {
	cal := "primary"
	if calendarID != nil {
		cal = *calendarID
	}
	return &Connector{
		datasourceID: datasourceID,
		credentials:  creds,
		calendarID:   cal,
		BaseURL:      APIBase,
		TokenURL:     oauth.GoogleTokenURL,
		Client:       &http.Client{},
	}
}

func (c *Connector) getToken(ctx context.Context) (string, error) {
	tok, err := oauth.GetValidToken(ctx, c.datasourceID, c.credentials, c.TokenURL)
	if err != nil {
		return "", connector.Auth("%s", err.Error())
	}
	return tok, nil
}

func (c *Connector) get(ctx context.Context, url, token string, transportErr func(string, ...any) *connector.Error) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, transportErr("%s", err.Error())
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, transportErr("%s", err.Error())
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, connector.FromUpstreamHTTP(resp.StatusCode, string(data), "Calendar API")
	}
	if err != nil {
		return nil, connector.Query("%s", err.Error())
	}
	return data, nil
}

type eventDateTime struct {
	DateTime *string `json:"dateTime"`
	Date     *string `json:"date"`
}

type calendarEvent struct {
	ID          *string        `json:"id"`
	Summary     *string        `json:"summary"`
	Description *string        `json:"description"`
	Location    *string        `json:"location"`
	Start       *eventDateTime `json:"start"`
	End         *eventDateTime `json:"end"`
	Status      *string        `json:"status"`
	Organizer   *struct {
		Email *string `json:"email"`
	} `json:"organizer"`
	Attendees []struct {
		Email          *string `json:"email"`
		ResponseStatus *string `json:"responseStatus"`
	} `json:"attendees"`
	HTMLLink *string `json:"htmlLink"`
	Created  *string `json:"created"`
	Updated  *string `json:"updated"`
}

func (c *Connector) Introspect(context.Context) (*connector.Schema, error) {
	f := func(name string, t connector.FieldType, desc string) connector.SchemaField {
		return connector.SchemaField{Name: name, FieldType: t, Description: models.Ptr(desc)}
	}
	return &connector.Schema{Fields: []connector.SchemaField{
		f("id", connector.FieldString, "Calendar event ID"),
		f("summary", connector.FieldString, "Event title"),
		f("description", connector.FieldString, "Event description"),
		f("location", connector.FieldString, "Event location"),
		f("start", connector.FieldDateTime, "Event start time"),
		f("end", connector.FieldDateTime, "Event end time"),
		f("status", connector.FieldString, "Event status (confirmed, tentative, cancelled)"),
		f("organizer", connector.FieldString, "Event organizer email"),
		f("attendees", connector.FieldJSON, "List of attendee emails and response status"),
		f("html_link", connector.FieldString, "Link to the event in Google Calendar"),
		f("created", connector.FieldDateTime, "When the event was created"),
		f("updated", connector.FieldDateTime, "When the event was last updated"),
	}}, nil
}

func (c *Connector) Query(ctx context.Context, params *connector.QueryParams) (*models.Envelope, error) {
	token, err := c.getToken(ctx)
	if err != nil {
		return nil, err
	}

	if params.ItemID != nil {
		url := fmt.Sprintf("%s/calendars/%s/events/%s", c.BaseURL, util.URLEncode(c.calendarID), util.URLEncode(*params.ItemID))
		data, err := c.get(ctx, url, token, connector.Query)
		if err != nil {
			return nil, err
		}
		var ev calendarEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, connector.Query("%s", err.Error())
		}
		return &models.Envelope{Items: []map[string]any{eventToJSON(&ev)}, Meta: models.EnvelopeMeta{Total: 1, Limit: 1}}, nil
	}

	// Push start/end bounds down to timeMin/timeMax; default timeMin to now so
	// the default listing is "upcoming events".
	timeMin, timeMax := extractTimeBounds(params.Filters)
	if timeMin == nil {
		timeMin = models.Ptr(formatRFC3339(now()))
	}
	url := fmt.Sprintf("%s/calendars/%s/events?maxResults=%d&singleEvents=true&orderBy=startTime&timeMin=%s",
		c.BaseURL, util.URLEncode(c.calendarID), params.Limit, util.URLEncode(*timeMin))
	if timeMax != nil {
		url += "&timeMax=" + util.URLEncode(*timeMax)
	}
	if params.Search != nil {
		url += "&q=" + util.URLEncode(*params.Search)
	}

	data, err := c.get(ctx, url, token, connector.Query)
	if err != nil {
		return nil, err
	}
	var list struct {
		Items         []calendarEvent `json:"items"`
		NextPageToken *string         `json:"nextPageToken"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, connector.Query("%s", err.Error())
	}
	items := make([]map[string]any, 0, len(list.Items))
	for i := range list.Items {
		items = append(items, eventToJSON(&list.Items[i]))
	}
	items = connector.ApplyFiltersInMemory(items, params.Filters)
	return &models.Envelope{Items: items, Meta: models.EnvelopeMeta{Total: int64(len(items)), Limit: params.Limit, Offset: params.Offset}}, nil
}

func (c *Connector) TestConnection(ctx context.Context) error {
	token, err := c.getToken(ctx)
	if err != nil {
		return err
	}
	_, err = c.get(ctx, fmt.Sprintf("%s/calendars/%s", c.BaseURL, util.URLEncode(c.calendarID)), token, connector.Connection)
	return err
}

func (c *Connector) GenerateSyntheticRows(_ []models.DatasetField, count int64) ([]map[string]any, bool) {
	return generateRows(count), true
}

// extractTimeBounds maps start/end filters onto Google's native bounds:
// `> X` → timeMin, `< X` → timeMax (last writer wins). The in-memory pass
// tightens the result afterwards. Date-only values get a midnight-UTC suffix.
func extractTimeBounds(filters []models.DatasetFilter) (timeMin, timeMax *string) {
	for _, f := range filters {
		if f.FieldName != "start" && f.FieldName != "end" {
			continue
		}
		val := normalizeRFC3339(util.ValueAsString(f.Value))
		switch f.Operator {
		case models.OpGreaterThan:
			timeMin = &val
		case models.OpLessThan:
			timeMax = &val
		}
	}
	return timeMin, timeMax
}

// normalizeRFC3339 expands `YYYY-MM-DD` to full RFC 3339 so Google accepts it.
func normalizeRFC3339(s string) string {
	if len(s) == 10 && s[4] == '-' && s[7] == '-' {
		return s + "T00:00:00Z"
	}
	return s
}

// epochToUTC converts Unix seconds to UTC civil time (Hinnant's civil-from-days).
func epochToUTC(secs int64) (y, mo, d, h, mi, s int64) {
	days := floorDiv(secs, 86400)
	sod := secs - days*86400
	h, mi, s = sod/3600, (sod%3600)/60, sod%60

	z := days + 719468
	era := floorDiv(z, 146097)
	doe := z - era*146097
	yoe := (doe - doe/1460 + doe/36524 - doe/146096) / 365
	y = yoe + era*400
	doy := doe - (365*yoe + yoe/4 - yoe/100)
	mp := (5*doy + 2) / 153
	d = doy - (153*mp+2)/5 + 1
	if mp < 10 {
		mo = mp + 3
	} else {
		mo = mp - 9
	}
	if mo <= 2 {
		y++
	}
	return
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b < 0 {
		q--
	}
	return q
}

func formatRFC3339(epoch int64) string {
	y, mo, d, h, mi, s := epochToUTC(epoch)
	return fmt.Sprintf("%04d-%02d-%02dT%02d:%02d:%02dZ", y, mo, d, h, mi, s)
}

func strOr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func eventTime(t *eventDateTime) string {
	if t == nil {
		return ""
	}
	if t.DateTime != nil {
		return *t.DateTime
	}
	return strOr(t.Date)
}

func eventToJSON(ev *calendarEvent) map[string]any {
	attendees := make([]any, 0, len(ev.Attendees))
	for _, a := range ev.Attendees {
		attendees = append(attendees, map[string]any{
			"email":           strOr(a.Email),
			"response_status": strOr(a.ResponseStatus),
		})
	}
	organizer := ""
	if ev.Organizer != nil {
		organizer = strOr(ev.Organizer.Email)
	}
	return map[string]any{
		"id":          strOr(ev.ID),
		"summary":     strOr(ev.Summary),
		"description": strOr(ev.Description),
		"location":    strOr(ev.Location),
		"start":       eventTime(ev.Start),
		"end":         eventTime(ev.End),
		"status":      strOr(ev.Status),
		"organizer":   organizer,
		"attendees":   attendees,
		"html_link":   strOr(ev.HTMLLink),
		"created":     strOr(ev.Created),
		"updated":     strOr(ev.Updated),
	}
}
