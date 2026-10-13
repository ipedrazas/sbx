package linear

import (
	"encoding/binary"
	"fmt"
	"math/bits"
	"time"
)

// ── Synthetic Linear data ──

type synthTeam struct{ key, name string }

var synthTeams = []synthTeam{
	{"ENG", "Engineering"},
	{"PLAT", "Platform"},
	{"DATA", "Data"},
	{"DESIGN", "Design"},
	{"GROW", "Growth"},
}

type synthProject struct{ name, description, state string }

var synthProjects = []synthProject{
	{"Q3 Platform Reliability", "Harden the platform: SLOs, alerting, and a tested failover runbook.", "started"},
	{"Mobile App Revamp", "Rebuild the mobile client on the new design system with offline support.", "planned"},
	{"Data Warehouse Migration", "Move analytics off the primary DB onto a dedicated warehouse.", "started"},
	{"SSO & Provisioning", "Ship SAML/SCIM so enterprise customers can self-serve user management.", "backlog"},
	{"Cost Optimization", "Cut cloud spend 30% via rightsizing, autoscaling, and storage tiering.", "completed"},
	{"Onboarding Funnel", "Reduce time-to-first-value for new signups end to end.", "started"},
}

var synthTitles = [][2]string{
	{"Login fails with SSO when session cookie is expired", "Steps to reproduce:\n1. Enable SSO\n2. Let the session cookie expire (~30 min idle)\n3. Try to log in again\n\nExpected: redirect to IdP for re-auth\nActual: 500 on /api/auth/callback"},
	{"Add CSV export to the analytics dashboard", "As a team lead I want to export dashboard data as CSV so I can share it with stakeholders who don't have app access."},
	{"Upgrade PostgreSQL from 14 to 16", "v14 goes EOL in November. Plan and execute the upgrade with minimal downtime using pglogical."},
	{"Search results don't update after removing a filter", "Removing a filter and re-fetching still shows the old filtered results. Cache isn't being cleared."},
	{"Implement dark mode for the settings page", "The settings page is the last view that ignores the system theme preference."},
	{"Set up alerting for API latency P99", "Alert when P99 crosses 500ms on any /api/* endpoint for 5 minutes."},
	{"Memory leak in WebSocket connection handler", "RSS grows ~50MB/hour under sustained connections. The connection map isn't cleaning up closed sockets."},
	{"Allow agents to specify preferred response format", "Support JSON, CSV, or plain text via an Accept header or ?format= query param."},
	{"Rotate production API keys before the audit", "Annual security audit requires key-rotation evidence. Rotate and update secrets in CI/CD."},
	{"Multi-region deployment support", "Deploy to EU-West alongside US-East to meet data-residency requirements for EU customers."},
	{"Duplicate webhook deliveries on retry", "Retries send the same payload twice — the idempotency check uses the wrong timestamp field."},
	{"Add Google Drive as a datasource connector", "List files and folders from Drive API v3 with OAuth and the drive.readonly scope."},
	{"Write a runbook for database failover", "Document promoting a read replica to primary: DNS cutover, pool drain, validation queries."},
	{"Timezone offset wrong in calendar exports", "Events created in UTC+2 show up 2 hours early in the exported ICS file — missing TZID."},
	{"Rate-limiting dashboard for operators", "Show which agents are hitting rate limits: name, endpoint, requests/min, limit, rejections."},
	{"Migrate CI from GitHub Actions to Buildkite", "GitHub Actions hits the 6-hour timeout on integration tests. Move for longer timeouts and caching."},
	{"Add unit tests for the PII scanner", "Cover partial SSN matches, card numbers with spaces, JWTs in query params, emails in JSON blobs."},
	{"OAuth refresh token rejected after DST changeover", "Expiry math uses wall-clock instead of monotonic time, so valid tokens get rejected at the changeover."},
	{"Synthetic data generation per connector type", "Generate realistic fake data per connector so synthetic datasets look real instead of generic."},
	{"Archive completed cycles older than 90 days", "The cycle picker has 47 entries. Auto-archive cycles closed for more than 90 days."},
}

var synthStates = []string{"Backlog", "Todo", "In Progress", "In Review", "Done", "Done", "In Progress", "Canceled"}

var synthProjectStates = []string{"backlog", "planned", "started", "paused", "completed"}

var synthPriorities = []string{"Urgent", "High", "Medium", "Medium", "Low", "No priority"}

var synthPeople = []string{
	"Alice Morgan", "Bob Chen", "Carol Diaz", "David Kim", "Eve Nakamura",
	"Frank Osei", "Grace Petrov", "Hank Rivera", "Ivy Johansson", "Jack Liu",
}

var synthLabels = [][]string{
	{"Bug"},
	{"Feature"},
	{"Bug", "Security"},
	{"Infra", "DevOps"},
	{"Feature", "UX"},
	{"Performance"},
	{"Bug", "Production"},
	{"Tech Debt"},
	{},
	{"Documentation"},
}

// sipHash13 is SipHash-1-3 with zero keys — Rust's DefaultHasher — so the
// synthetic rows are byte-identical to the Rust implementation's.
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
	b := uint64(n&0xff) << 56
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

// synthHash mirrors hashing (row: i64, salt: u64) with DefaultHasher.
func synthHash(row int64, salt uint64) uint64 {
	var buf [16]byte
	binary.LittleEndian.PutUint64(buf[:8], uint64(row))
	binary.LittleEndian.PutUint64(buf[8:], salt)
	return sipHash13(buf[:])
}

// synthTeamID is a stable fake id for a team key, shared by issues and teams.
func synthTeamID(key string) string {
	// Rust's str Hash writes the bytes plus a 0xff terminator.
	h := sipHash13(append([]byte(key), 0xff))
	return fmt.Sprintf("team_%012x", h&0xffff_ffff_ffff)
}

// synthProjectID is a stable fake id for a synthetic project by index.
func synthProjectID(idx int64) string {
	return fmt.Sprintf("proj_%012x", synthHash(idx, 0xC0FFEE)&0xffff_ffff_ffff)
}

func synthRFC3339(epoch int64) string {
	return time.Unix(epoch, 0).UTC().Format("2006-01-02T15:04:05.000Z")
}

// synthStateType maps a synthetic state name to its WorkflowState.type.
func synthStateType(state string) string {
	switch state {
	case "Backlog":
		return "backlog"
	case "Todo":
		return "unstarted"
	case "Done":
		return "completed"
	case "Canceled":
		return "canceled"
	}
	return "started"
}

func pick(h uint64, n int) int { return int(h % uint64(n)) }

func strsToAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

const synthBaseTS int64 = 1746100800 // ~2025-05-01 12:00 UTC

func generateIssueRows(count int64) []map[string]any {
	rows := make([]map[string]any, 0, max(count, 0))
	for i := int64(0); i < count; i++ {
		team := synthTeams[pick(synthHash(i, 0), len(synthTeams))]
		title := synthTitles[pick(synthHash(i, 1), len(synthTitles))]
		state := synthStates[pick(synthHash(i, 2), len(synthStates))]
		prio := synthPriorities[pick(synthHash(i, 3), len(synthPriorities))]
		assignee := synthPeople[pick(synthHash(i, 4), len(synthPeople))]
		creator := synthPeople[pick(synthHash(i, 5), len(synthPeople))]
		labels := synthLabels[pick(synthHash(i, 6), len(synthLabels))]
		// One slot past the project list means "no project".
		projectSlot := pick(synthHash(i, 10), len(synthProjects)+1)

		identifier := fmt.Sprintf("%s-%d", team.key, uint32(i+1))
		projectName, projectID := "", ""
		if projectSlot < len(synthProjects) {
			projectName, projectID = synthProjects[projectSlot].name, synthProjectID(int64(projectSlot))
		}

		createdTS := synthBaseTS - int64(synthHash(i, 7)%5_184_000+86400) // 1-60 days ago
		updatedTS := createdTS + int64(synthHash(i, 8)%uint64(max(synthBaseTS-createdTS, 1)))

		rows = append(rows, map[string]any{
			"id":          fmt.Sprintf("issue_%08x", synthHash(i, 9)),
			"identifier":  identifier,
			"title":       title[0],
			"status":      state,
			"state_type":  synthStateType(state),
			"priority":    prio,
			"assignee":    assignee,
			"creator":     creator,
			"team":        team.key,
			"team_id":     synthTeamID(team.key),
			"project":     projectName,
			"project_id":  projectID,
			"created":     synthRFC3339(createdTS),
			"updated":     synthRFC3339(updatedTS),
			"labels":      strsToAny(labels),
			"description": title[1],
			"url":         "https://linear.app/acme/issue/" + identifier,
		})
	}
	return rows
}

func generateProjectRows(count int64) []map[string]any {
	n := int64(len(synthProjects))
	rows := make([]map[string]any, 0, max(count, 0))
	for i := int64(0); i < count; i++ {
		projIdx := i % n
		proj := synthProjects[projIdx]
		lead := synthPeople[pick(synthHash(i, 1), len(synthPeople))]
		team := synthTeams[pick(synthHash(i, 2), len(synthTeams))]
		// Canonical state for the first cycle, then vary on repeats.
		state, name, idIdx := proj.state, proj.name, projIdx
		if i >= n {
			state = synthProjectStates[pick(synthHash(i, 3), len(synthProjectStates))]
			name = fmt.Sprintf("%s (%d)", proj.name, i+1)
			idIdx = i
		}
		progress := float64(synthHash(i, 4)%101) / 100.0

		createdTS := synthBaseTS - int64(synthHash(i, 7)%15_552_000+86400) // up to ~180d
		updatedTS := createdTS + int64(synthHash(i, 8)%uint64(max(synthBaseTS-createdTS, 1)))

		rows = append(rows, map[string]any{
			"id":          synthProjectID(idIdx),
			"name":        name,
			"status":      state,
			"progress":    progress,
			"lead":        lead,
			"teams":       []any{team.key},
			"created":     synthRFC3339(createdTS),
			"updated":     synthRFC3339(updatedTS),
			"description": proj.description,
			"url":         "https://linear.app/acme/project/" + synthProjectID(projIdx),
		})
	}
	return rows
}

func generateTeamRows(count int64) []map[string]any {
	rows := make([]map[string]any, 0, max(count, 0))
	for i := int64(0); i < count; i++ {
		t := synthTeams[i%int64(len(synthTeams))]
		rows = append(rows, map[string]any{
			"id":          synthTeamID(t.key),
			"key":         t.key,
			"name":        t.name,
			"description": fmt.Sprintf("The %s team", t.name),
		})
	}
	return rows
}
