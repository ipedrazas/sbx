package jira

import (
	"encoding/binary"
	"fmt"
	"math/bits"
	"time"
)

// ── Synthetic Jira data ──

var synthProjects = []string{"ENG", "PLAT", "DATA", "INFRA", "MOBILE"}

var synthSummaries = [][3]string{
	{"Bug", "Login fails with SSO when session cookie is expired", "Steps to reproduce:\n1. Enable SSO in workspace settings\n2. Wait for the session cookie to expire (~30 min idle)\n3. Try to log in again\n\nExpected: redirect to IdP for re-auth\nActual: 500 error on /api/auth/callback"},
	{"Story", "Add CSV export to the analytics dashboard", "As a team lead, I want to export dashboard data as CSV so I can share it with stakeholders who don't have app access.\n\nAcceptance criteria:\n- Export button on the top-right of the dashboard\n- Includes all visible columns and applied filters\n- File name: {dashboard_name}_{date}.csv"},
	{"Task", "Upgrade PostgreSQL from 14 to 16", "Our managed Postgres instances are on v14 which goes EOL in November. Need to plan and execute the upgrade.\n\nSteps:\n1. Test on staging with pg_dump/pg_restore\n2. Schedule maintenance window\n3. Run upgrade with pglogical for minimal downtime\n4. Verify replication lag is zero"},
	{"Bug", "Search results don't update after removing a filter", "When a user applies a filter, fetches results, then removes the filter and re-fetches, the old filtered results still show. Must clear the cache or re-query without the removed filter."},
	{"Story", "Implement dark mode for the settings page", "Design spec: https://figma.com/file/xyz\n\nThe settings page is the last view that doesn't respect the system theme preference. Align it with the rest of the app."},
	{"Task", "Set up Datadog monitors for API latency P99", "We need alerting when P99 latency crosses 500ms on any /api/* endpoint. Create monitors in Datadog with:\n- Warning: P99 > 300ms for 5 min\n- Critical: P99 > 500ms for 5 min\n- Notification: #eng-oncall Slack channel"},
	{"Bug", "Memory leak in WebSocket connection handler", "The daemon's RSS grows ~50MB/hour under sustained WebSocket connections. Suspect the connection map isn't cleaning up closed sockets. Heap profile attached."},
	{"Story", "Allow agents to specify preferred response format", "Agents should be able to request JSON, CSV, or plain text responses from the dataset API via an Accept header or ?format= query param."},
	{"Task", "Rotate production API keys before Q3 audit", "Annual security audit requires key rotation evidence. Rotate all production API keys and update the secrets in Vault and CI/CD."},
	{"Epic", "Multi-region deployment support", "Deploy the platform to EU-West in addition to US-East to meet data residency requirements for EU customers. This is a multi-sprint effort spanning infra, data, and application layers."},
	{"Bug", "Duplicate webhook deliveries on retry", "Webhook retries are sending the same payload twice — the idempotency check uses the wrong timestamp field. Consumers are processing duplicate events."},
	{"Story", "Add Google Drive as a datasource connector", "Implement a connector that lists files and folders from Google Drive via the Drive API v3. Support OAuth with drive.readonly scope."},
	{"Task", "Write runbook for database failover procedure", "Document the step-by-step process for promoting a read replica to primary, including DNS cutover, connection pool drain, and validation queries."},
	{"Bug", "Timezone offset wrong in calendar event exports", "Events created in UTC+2 show up 2 hours early in the exported ICS file. The RFC 5545 DTSTART is missing the TZID component."},
	{"Story", "Rate limiting dashboard for operators", "Operators need visibility into which agents are hitting rate limits. Add a view showing: agent name, endpoint, requests/minute, limit, and rejection count over the last hour."},
	{"Task", "Migrate CI pipeline from GitHub Actions to Buildkite", "GitHub Actions is hitting the 6-hour job timeout on our integration test suite. Move to Buildkite for longer timeouts and better caching."},
	{"Sub-task", "Add unit tests for the PII scanner", "Cover the edge cases: partial SSN matches, credit card numbers with spaces, JWTs in query params, emails in JSON blobs."},
	{"Bug", "OAuth refresh token rejected after DST changeover", "Token expiry calculation uses wall-clock math instead of monotonic time. When clocks spring forward, tokens that should still be valid get rejected."},
	{"Story", "Synthetic data generation per connector type", "Generate realistic fake data for each connector so synthetic datasets look like real Gmail messages, Jira issues, etc. instead of generic placeholder text."},
	{"Task", "Archive completed sprints older than 90 days", "The sprint picker dropdown has 47 entries. Auto-archive sprints that have been closed for > 90 days to keep the list manageable."},
}

var synthStatuses = []string{"To Do", "In Progress", "In Review", "Done", "Done", "In Progress", "To Do", "Blocked"}

var synthPriorities = []string{"Highest", "High", "Medium", "Medium", "Medium", "Low", "Low", "Lowest"}

var synthPeople = []string{
	"Alice Morgan", "Bob Chen", "Carol Diaz", "David Kim", "Eve Nakamura",
	"Frank Osei", "Grace Petrov", "Hank Rivera", "Ivy Johansson", "Jack Liu",
}

var synthLabels = [][]string{
	{"backend"},
	{"frontend"},
	{"backend", "security"},
	{"infra", "devops"},
	{"frontend", "ux"},
	{"backend", "performance"},
	{"bug", "production"},
	{"tech-debt"},
	{},
	{"documentation"},
	{"backend", "api"},
	{"mobile", "ios"},
}

// synthHash reproduces Rust's `DefaultHasher` (SipHash-1-3, zero keys) over
// `row.hash(); salt.hash()` so synthetic rows match the Rust daemon exactly.
func synthHash(row int64, salt uint64) uint64 {
	var msg [16]byte
	binary.LittleEndian.PutUint64(msg[:8], uint64(row))
	binary.LittleEndian.PutUint64(msg[8:], salt)
	return sipHash13(msg[:])
}

func sipHash13(msg []byte) uint64 {
	v0 := uint64(0x736f6d6570736575)
	v1 := uint64(0x646f72616e646f6d)
	v2 := uint64(0x6c7967656e657261)
	v3 := uint64(0x7465646279746573)
	round := func() {
		v0 += v1
		v1 = bits.RotateLeft64(v1, 13)
		v1 ^= v0
		v0 = bits.RotateLeft64(v0, 32)
		v2 += v3
		v3 = bits.RotateLeft64(v3, 16)
		v3 ^= v2
		v0 += v3
		v3 = bits.RotateLeft64(v3, 21)
		v3 ^= v0
		v2 += v1
		v1 = bits.RotateLeft64(v1, 17)
		v1 ^= v2
		v2 = bits.RotateLeft64(v2, 32)
	}
	n := len(msg)
	i := 0
	for ; i+8 <= n; i += 8 {
		m := binary.LittleEndian.Uint64(msg[i:])
		v3 ^= m
		round()
		v0 ^= m
	}
	b := uint64(n) << 56
	for j := 0; i+j < n; j++ {
		b |= uint64(msg[i+j]) << (8 * j)
	}
	v3 ^= b
	round()
	v0 ^= b
	v2 ^= 0xff
	round()
	round()
	round()
	return v0 ^ v1 ^ v2 ^ v3
}

func synthRFC3339(epoch int64) string {
	return time.Unix(epoch, 0).UTC().Format("2006-01-02T15:04:05.000-0700")
}

func strSlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func generateJiraRows(count int64) []map[string]any {
	const baseTS int64 = 1746100800 // ~2025-05-01 12:00 UTC
	rows := make([]map[string]any, 0, max(count, 0))
	pick := func(i int64, salt uint64, n int) int { return int(synthHash(i, salt) % uint64(n)) }
	for i := int64(0); i < count; i++ {
		project := synthProjects[pick(i, 0, len(synthProjects))]
		summ := synthSummaries[pick(i, 1, len(synthSummaries))]
		issueNum := uint32(i + 1)

		createdTS := baseTS - int64(synthHash(i, 7)%5_184_000+86400) // 1-60 days ago
		updatedTS := createdTS + int64(synthHash(i, 8)%uint64(max(baseTS-createdTS, 1)))

		rows = append(rows, map[string]any{
			"id":          fmt.Sprintf("%d", 10000+issueNum),
			"key":         fmt.Sprintf("%s-%d", project, issueNum),
			"summary":     summ[1],
			"status":      synthStatuses[pick(i, 2, len(synthStatuses))],
			"issue_type":  summ[0],
			"priority":    synthPriorities[pick(i, 3, len(synthPriorities))],
			"assignee":    synthPeople[pick(i, 4, len(synthPeople))],
			"reporter":    synthPeople[pick(i, 5, len(synthPeople))],
			"project":     project,
			"created":     synthRFC3339(createdTS),
			"updated":     synthRFC3339(updatedTS),
			"labels":      strSlice(synthLabels[pick(i, 6, len(synthLabels))]),
			"description": summ[2],
		})
	}
	return rows
}
