package gmail

import "fmt"

var synthSenders = [][2]string{
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
	{"GitHub", "notifications@github.com"},
	{"Google Calendar", "calendar-notification@google.com"},
	{"Jira", "jira@acmecorp.atlassian.net"},
	{"Slack", "no-reply@slack.com"},
	{"Linear", "notifications@linear.app"},
}

var synthSubjects = []string{
	"Q3 roadmap review — action items",
	"Re: Updated design specs for dashboard",
	"Invoice #4821 attached",
	"Meeting notes: Product sync 2025-04-28",
	"New pull request: fix auth token refresh",
	"Your weekly project digest",
	"Quick question about the API migration",
	"Invitation: Sprint planning @ Mon 10:00",
	"Re: Customer feedback — priority issues",
	"Deployment to staging completed successfully",
	"Expense report submitted for approval",
	"Re: Can we reschedule tomorrow's call?",
	"Welcome to the team!",
	"Security alert: new sign-in from Chrome on macOS",
	"[Bug] Users seeing 500 on /api/settings",
	"Your subscription renewal is coming up",
	"Re: Thoughts on the new onboarding flow?",
	"Monthly analytics report — April 2025",
	"Out of office: May 5-9",
	"Feedback requested: draft blog post",
}

var synthSnippets = []string{
	"Hi team, here are the action items from today's roadmap review. Please update your tasks by EOD Friday...",
	"Thanks for the update. I've left some comments on the Figma file — mostly around the nav spacing...",
	"Please find the invoice for April attached. Payment is due within 30 days. Let me know if you have...",
	"Summary of what we discussed: 1) Launch timeline moved to June 15. 2) Design hand-off by May 10...",
	"This PR fixes the token refresh race condition we saw in production last week. Added a mutex around...",
	"Here's what happened in your projects this week: 14 tasks completed, 3 new issues filed, 2 PRs merged...",
	"Hey, quick one — are we keeping the v1 endpoints after the migration or deprecating them? Need to know...",
	"You've been invited to Sprint Planning on Monday at 10:00 AM. Agenda: capacity review, backlog grooming...",
	"Forwarding the top customer issues from last week. The auth timeout and slow search are the most urgent...",
	"Build #847 deployed to staging at 14:32 UTC. All 312 tests passed. No regressions detected in smoke...",
	"I've submitted my expenses from the London trip. Total: £482.30. Receipts attached for hotel and...",
	"No worries at all! How about Thursday at 3pm instead? I'll send a calendar invite once confirmed...",
	"We're so excited to have you join! Your accounts have been set up. Here's what to do on your first day...",
	"A new sign-in to your account was detected from Chrome on macOS (London, UK). If this was you, no action...",
	"Seeing intermittent 500s on the settings endpoint since this morning's deploy. Stack trace points to a nil...",
	"Your annual subscription renews on May 20. Your card ending in 4242 will be charged $299. Update payment...",
	"I really like the progressive disclosure approach. One concern: the tooltip on step 3 might be confusing...",
	"Highlights: 23% increase in signups, average session duration up 1.2 minutes, bounce rate down to 38%...",
	"I'll be out of office May 5-9 with limited email access. For urgent matters, please contact Sarah at...",
	"Draft is in the shared Drive folder. Would love your thoughts on the intro section especially — I'm not...",
}

var synthLabels = [][]string{
	{"INBOX", "IMPORTANT", "CATEGORY_PERSONAL"},
	{"INBOX", "CATEGORY_UPDATES"},
	{"INBOX", "CATEGORY_PERSONAL"},
	{"INBOX", "IMPORTANT", "STARRED"},
	{"INBOX", "CATEGORY_UPDATES"},
	{"INBOX", "CATEGORY_UPDATES", "UNREAD"},
	{"INBOX", "CATEGORY_PERSONAL"},
	{"INBOX", "CATEGORY_UPDATES"},
	{"INBOX", "IMPORTANT", "CATEGORY_PERSONAL"},
	{"INBOX", "CATEGORY_UPDATES"},
	{"INBOX", "CATEGORY_PERSONAL"},
	{"INBOX", "CATEGORY_PERSONAL"},
	{"INBOX", "IMPORTANT", "STARRED", "CATEGORY_PERSONAL"},
	{"INBOX", "IMPORTANT", "CATEGORY_UPDATES"},
	{"INBOX", "IMPORTANT", "UNREAD", "CATEGORY_UPDATES"},
	{"INBOX", "CATEGORY_UPDATES"},
	{"INBOX", "CATEGORY_PERSONAL"},
	{"INBOX", "CATEGORY_UPDATES"},
	{"INBOX", "CATEGORY_PERSONAL"},
	{"INBOX", "CATEGORY_PERSONAL"},
}

func idx(h uint64, n int) int { return int(h % uint64(n)) }

func generateRows(count int64) []map[string]any {
	const baseTS = int64(1746000000) // ~2025-04-30
	rows := make([]map[string]any, 0, max(count, 0))
	for i := int64(0); i < count; i++ {
		sender := synthSenders[idx(synthHash(i, 0x674D41), len(synthSenders))]
		subject := synthSubjects[idx(synthHash(i, 1), len(synthSubjects))]
		snippet := synthSnippets[idx(synthHash(i, 2), len(synthSnippets))]
		labelSet := synthLabels[idx(synthHash(i, 3), len(synthLabels))]

		// Spread dates backwards from base, ~2-8 hours apart.
		offset := i * int64(synthHash(i, 4)%21600+7200)
		labels := make([]any, len(labelSet))
		for j, l := range labelSet {
			labels[j] = l
		}
		rows = append(rows, map[string]any{
			"id":        fmt.Sprintf("%016x", synthHash(i, 5)),
			"thread_id": fmt.Sprintf("%016x", synthHash(i, 6)),
			"from":      fmt.Sprintf("%s <%s>", sender[0], sender[1]),
			"to":        "me",
			"subject":   subject,
			"date":      chronoLikeFormat(baseTS - offset),
			"snippet":   snippet,
			"label_ids": labels,
		})
	}
	return rows
}

// chronoLikeFormat renders an RFC 2822-style date ("Wed, 30 Apr 2025 08:00:00 +0000").
func chronoLikeFormat(epoch int64) string {
	daysPerMonth := [12]int64{31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}
	monthNames := [12]string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}
	dayNames := [7]string{"Thu", "Fri", "Sat", "Sun", "Mon", "Tue", "Wed"} // epoch 0 = Thursday

	days := epoch / 86400
	daySecs := epoch % 86400
	hour, minute, second := daySecs/3600, (daySecs%3600)/60, daySecs%60
	dow := (days%7 + 7) % 7

	isLeap := func(y int64) bool { return y%4 == 0 && (y%100 != 0 || y%400 == 0) }
	year := int64(1970)
	for {
		yd := int64(365)
		if isLeap(year) {
			yd = 366
		}
		if days < yd {
			break
		}
		days -= yd
		year++
	}
	month := 0
	for m := 0; m < 12; m++ {
		md := daysPerMonth[m]
		if m == 1 && isLeap(year) {
			md = 29
		}
		if days < md {
			month = m
			break
		}
		days -= md
	}
	return fmt.Sprintf("%s, %d %s %d %02d:%02d:%02d +0000", dayNames[dow], days+1, monthNames[month], year, hour, minute, second)
}
