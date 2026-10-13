package gcal

import "fmt"

var synthPeople = [][2]string{
	{"Alice Morgan", "alice.morgan@example.com"},
	{"Bob Chen", "bob.chen@acmecorp.io"},
	{"Carol Diaz", "carol.diaz@startup.dev"},
	{"David Kim", "david.kim@bigco.com"},
	{"Eve Nakamura", "eve.nakamura@consulting.org"},
	{"Frank Osei", "frank.osei@freelance.net"},
	{"Grace Petrov", "grace.petrov@design.studio"},
	{"Hank Rivera", "hank.rivera@sales.team"},
	{"Ivy Johansson", "ivy.johansson@support.help"},
	{"Jack Liu", "jack.liu@engineering.dev"},
}

type synthEvent struct {
	title, desc string
	durationMin int64
}

var synthEvents = []synthEvent{
	{"Sprint Planning", "Review capacity, groom backlog, assign stories for the next sprint.", 60},
	{"1:1 with Manager", "Weekly sync — career growth, blockers, feedback.", 30},
	{"Design Review: Dashboard v2", "Walk through the updated Figma mocks. Focus on the nav redesign and mobile breakpoints.", 45},
	{"Team Standup", "Quick round: what you did, what you're doing, any blockers.", 15},
	{"Product Roadmap Review", "Q3 priorities, feature trade-offs, resourcing.", 90},
	{"Customer Demo: Acme Corp", "Live demo of the new reporting module. Prep: load staging with sample data.", 60},
	{"Incident Retro: API Outage 04/22", "Timeline review, root cause, action items. Blameless format.", 45},
	{"Lunch & Learn: Rust Async", "Informal talk on async/await patterns. Bring your lunch!", 60},
	{"Interview: Senior Backend Engineer", "System design round. Candidate: Jamie Park.", 60},
	{"All-Hands Meeting", "Company update, Q2 results, new hires introduction.", 60},
	{"Architecture Decision: Auth Migration", "Discuss moving from session cookies to JWT. Bring concerns.", 45},
	{"Offsite Planning Committee", "Logistics for the June team offsite: venue, agenda, budget.", 30},
	{"Release Go/No-Go: v2.4", "Final checklist before cutting the release branch. QA sign-off required.", 30},
	{"Focus Time", "", 120},
	{"Coffee Chat: New Hire Onboarding", "Casual meet & greet with the latest joiners.", 30},
	{"Security Review: OAuth Scopes", "Audit current OAuth scope usage across all connectors.", 45},
	{"Board Prep: Metrics Deep Dive", "Prepare slides for ARR, churn, and NPS trends.", 60},
	{"Yoga / Stretch Break", "Mid-afternoon wellness break. Camera optional.", 15},
	{"Quarterly OKR Check-in", "Score existing OKRs, flag at-risk items, propose adjustments.", 60},
	{"Hackathon Kickoff", "24-hour hackathon. Theme: internal tooling. Teams of 2-4.", 30},
}

var synthLocations = []string{
	"Conference Room A (Floor 3)",
	"Conference Room B (Floor 2)",
	"Zoom — link in description",
	"Google Meet",
	"Main Boardroom",
	"",
	"Café area, 1st floor",
	"Remote — Slack huddle",
	"",
	"Room 401 (Rooftop)",
}

var synthResponses = []string{"accepted", "accepted", "accepted", "tentative", "needsAction", "declined"}

func idx(h uint64, n int) int { return int(h % uint64(n)) }

func generateRows(count int64) []map[string]any {
	const baseTS = int64(1746100800) // ~2025-05-01 12:00 UTC
	rows := make([]map[string]any, 0, max(count, 0))
	for i := int64(0); i < count; i++ {
		ev := synthEvents[idx(synthHash(i, 0), len(synthEvents))]
		loc := synthLocations[idx(synthHash(i, 1), len(synthLocations))]
		orgIdx := idx(synthHash(i, 2), len(synthPeople))

		// ~4 events per day across workdays (skip weekends), 9:00-16:45 slots.
		dayOffset := i / 4
		slotHour := 9 + int64(synthHash(i, 3)%8)
		slotMin := int64(synthHash(i, 4)%4) * 15
		actualDay := (dayOffset/5)*7 + dayOffset%5

		startTS := baseTS + actualDay*86400 + slotHour*3600 + slotMin*60
		endTS := startTS + ev.durationMin*60
		createdTS := startTS - int64(synthHash(i, 5)%604800+86400) // 1-7 days before

		org := synthPeople[orgIdx]

		// 1-4 attendees, never the organizer.
		n := 1 + int(synthHash(i, 6)%4)
		attendees := make([]any, 0, n)
		for a := 0; a < n; a++ {
			pi := idx(synthHash(i, 10+uint64(a)), len(synthPeople))
			if pi == orgIdx {
				pi = (pi + 1) % len(synthPeople)
			}
			attendees = append(attendees, map[string]any{
				"email":           synthPeople[pi][1],
				"response_status": synthResponses[idx(synthHash(i, 20+uint64(a)), len(synthResponses))],
			})
		}

		status := "confirmed"
		if synthHash(i, 7)%20 == 0 {
			status = "cancelled"
		}
		idHex := fmt.Sprintf("%024x", synthHash(i, 8))

		rows = append(rows, map[string]any{
			"id":          idHex,
			"summary":     ev.title,
			"description": ev.desc,
			"location":    loc,
			"start":       formatRFC3339(startTS),
			"end":         formatRFC3339(endTS),
			"status":      status,
			"organizer":   fmt.Sprintf("%s <%s>", org[0], org[1]),
			"attendees":   attendees,
			"html_link":   "https://calendar.google.com/calendar/event?eid=" + idHex[:16],
			"created":     formatRFC3339(createdTS),
			"updated":     formatRFC3339(createdTS + 3600),
		})
	}
	return rows
}
