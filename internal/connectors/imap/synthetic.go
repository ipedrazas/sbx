package imap

import (
	"encoding/binary"
	"fmt"
	"math/bits"
	"strconv"
)

var synthSenders = [][2]string{
	{"Alice Morgan", "alice.morgan@example.com"},
	{"Bob Chen", "bob.chen@acmecorp.io"},
	{"Carol Diaz", "carol.diaz@startup.dev"},
	{"David Kim", "david.kim@bigco.com"},
	{"Eve Nakamura", "eve.nakamura@consulting.org"},
	{"Frank Osei", "frank.osei@freelance.net"},
	{"Grace Petrov", "grace.petrov@design.studio"},
	{"Hank Rivera", "hank.rivera@sales.team"},
	{"GitHub", "notifications@github.com"},
	{"Jira", "jira@acmecorp.atlassian.net"},
}

var synthSubjects = []string{
	"Q3 roadmap review — action items",
	"Re: Updated design specs for dashboard",
	"Invoice #4821 attached",
	"Meeting notes: Product sync 2025-04-28",
	"New pull request: fix auth token refresh",
	"Your weekly project digest",
	"Quick question about the API migration",
	"Re: Customer feedback — priority issues",
	"Deployment to staging completed successfully",
	"Expense report submitted for approval",
}

var synthSnippets = []string{
	"Hi team, here are the action items from today's roadmap review...",
	"Thanks for the update. I've left some comments on the Figma file...",
	"Please find the invoice for April attached. Payment is due within 30 days...",
	"Summary of what we discussed: 1) Launch timeline moved to June 15...",
	"This PR fixes the token refresh race condition we saw in production...",
	"Here's what happened in your projects this week: 14 tasks completed...",
	"Hey, quick one — are we keeping the v1 endpoints after the migration?...",
	"Forwarding the top customer issues from last week...",
	"Build #847 deployed to staging at 14:32 UTC. All 312 tests passed...",
	"I've submitted my expenses from the London trip. Total: £482.30...",
}

var synthFlags = [][]string{
	{"\\Seen"},
	{"\\Seen", "\\Flagged"},
	{"\\Recent"},
	{"\\Seen", "\\Answered"},
	{"\\Recent"},
	{"\\Seen"},
	{"\\Seen", "\\Flagged", "\\Answered"},
	{"\\Recent"},
	{"\\Seen"},
	{"\\Seen", "\\Answered"},
}

// synthHash reproduces Rust's `DefaultHasher::new()` (SipHash-1-3, zero
// keys) over `row.hash(); salt.hash()`, so synthetic rows match the Rust
// daemon byte for byte.
func synthHash(row int64, salt uint64) uint64 {
	var msg [16]byte
	binary.LittleEndian.PutUint64(msg[:8], uint64(row))
	binary.LittleEndian.PutUint64(msg[8:], salt)
	return sipHash13(0, 0, msg[:])
}

func sipHash13(k0, k1 uint64, msg []byte) uint64 {
	v0 := k0 ^ 0x736f6d6570736575
	v1 := k1 ^ 0x646f72616e646f6d
	v2 := k0 ^ 0x6c7967656e657261
	v3 := k1 ^ 0x7465646279746573
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
	for len(msg) >= 8 {
		m := binary.LittleEndian.Uint64(msg)
		v3 ^= m
		round()
		v0 ^= m
		msg = msg[8:]
	}
	b := uint64(n) << 56
	for i, c := range msg {
		b |= uint64(c) << (8 * i)
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

func generateIMAPRows(count int64) []map[string]any {
	const baseTS = int64(1746000000)
	rows := make([]map[string]any, 0, max(count, 0))
	for i := int64(0); i < count; i++ {
		sender := synthSenders[synthHash(i, 0x494D4150)%uint64(len(synthSenders))]
		subj := synthSubjects[synthHash(i, 1)%uint64(len(synthSubjects))]
		snip := synthSnippets[synthHash(i, 2)%uint64(len(synthSnippets))]
		flags := synthFlags[synthHash(i, 3)%uint64(len(synthFlags))]

		offsetSecs := i * int64(synthHash(i, 4)%21600+7200)
		ts := baseTS - offsetSecs
		day := (ts/86400)%28 + 1
		hour := (ts % 86400) / 3600
		minute := (ts % 3600) / 60

		fl := make([]any, len(flags))
		for j, f := range flags {
			fl[j] = f
		}
		rows = append(rows, map[string]any{
			"id":         strconv.FormatUint(uint64(count-i), 10),
			"message_id": fmt.Sprintf("<%016x@mail.example.com>", synthHash(i, 5)),
			"from":       fmt.Sprintf("%s <%s>", sender[0], sender[1]),
			"to":         "me@example.com",
			"subject":    subj,
			"date":       fmt.Sprintf("2025-04-%02dT%02d:%02d:00+00:00", day, hour, minute),
			"snippet":    snip,
			"flags":      fl,
		})
	}
	return rows
}
