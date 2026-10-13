package gdrive

import (
	"fmt"
	"strings"
)

type synthFile struct{ name, mime, size string }

var synthFiles = []synthFile{
	{"Q3 Roadmap", "application/vnd.google-apps.document", ""},
	{"Sprint Retro Notes — May 2025", "application/vnd.google-apps.document", ""},
	{"API Migration Plan", "application/vnd.google-apps.document", ""},
	{"Team OKRs 2025", "application/vnd.google-apps.spreadsheet", ""},
	{"Hiring Pipeline", "application/vnd.google-apps.spreadsheet", ""},
	{"Revenue Dashboard — April", "application/vnd.google-apps.spreadsheet", ""},
	{"Product Launch Deck", "application/vnd.google-apps.presentation", ""},
	{"Board Meeting — Q2 Update", "application/vnd.google-apps.presentation", ""},
	{"Customer Onboarding Flow", "application/vnd.google-apps.presentation", ""},
	{"Engineering", "application/vnd.google-apps.folder", ""},
	{"Design Assets", "application/vnd.google-apps.folder", ""},
	{"Shared with Clients", "application/vnd.google-apps.folder", ""},
	{"SOC 2 Compliance Report.pdf", "application/pdf", "2841600"},
	{"NDA — Acme Corp.pdf", "application/pdf", "185400"},
	{"Invoice #4821.pdf", "application/pdf", "94208"},
	{"architecture-diagram.png", "image/png", "482304"},
	{"logo-dark.svg", "image/svg+xml", "12480"},
	{"team-photo-offsite.jpg", "image/jpeg", "3145728"},
	{"database-backup-2025-04-28.sql.gz", "application/gzip", "15728640"},
	{"user-research-interviews.mp4", "video/mp4", "524288000"},
	{"Meeting Recording — Design Sync.mp4", "video/mp4", "104857600"},
	{"export-2025-04.csv", "text/csv", "1048576"},
	{"README.md", "text/markdown", "4096"},
	{"config.json", "application/json", "2048"},
}

var synthOwners = []string{
	"alice.morgan@example.com",
	"bob.chen@acmecorp.io",
	"carol.diaz@startup.dev",
	"david.kim@bigco.com",
	"eve.nakamura@consulting.org",
	"frank.osei@freelance.net",
	"grace.petrov@design.studio",
	"hank.rivera@sales.team",
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b < 0 {
		q--
	}
	return q
}

// synthRFC3339 mirrors the Rust helper (truncating day split, Hinnant civil-from-days).
func synthRFC3339(epoch int64) string {
	days := epoch / 86400
	secs := epoch % 86400
	h, m, s := secs/3600, (secs%3600)/60, secs%60

	z := days + 719468
	era := floorDiv(z, 146097)
	doe := z - era*146097
	yoe := (doe - doe/1460 + doe/36524 - doe/146096) / 365
	y := yoe + era*400
	doy := doe - (365*yoe + yoe/4 - yoe/100)
	mp := (5*doy + 2) / 153
	d := doy - (153*mp+2)/5 + 1
	mo := mp + 3
	if mp >= 10 {
		mo = mp - 9
	}
	if mo <= 2 {
		y++
	}
	return fmt.Sprintf("%04d-%02d-%02dT%02d:%02d:%02dZ", y, mo, d, h, m, s)
}

func generateRows(count int64) []map[string]any {
	const baseTS = int64(1746100800) // ~2025-05-01 12:00 UTC
	rows := make([]map[string]any, 0, max(count, 0))
	for i := int64(0); i < count; i++ {
		sf := synthFiles[synthHash(i, 0)%uint64(len(synthFiles))]
		owner := synthOwners[synthHash(i, 1)%uint64(len(synthOwners))]

		createdTS := baseTS - int64(synthHash(i, 2)%7_776_000+86400) // 1-90 days ago
		modifiedTS := createdTS + int64(synthHash(i, 3)%uint64(max(baseTS-createdTS, 1)))

		idHex := fmt.Sprintf("%028x", synthHash(i, 4))
		shared := synthHash(i, 5)%3 != 0  // ~67% shared
		starred := synthHash(i, 6)%8 == 0 // ~12% starred

		var link string
		switch {
		case sf.mime == "application/vnd.google-apps.folder":
			link = "https://drive.google.com/drive/folders/" + idHex[:20]
		case strings.HasPrefix(sf.mime, "application/vnd.google-apps"):
			app := "file"
			switch sf.mime {
			case "application/vnd.google-apps.document":
				app = "document"
			case "application/vnd.google-apps.spreadsheet":
				app = "spreadsheets"
			case "application/vnd.google-apps.presentation":
				app = "presentation"
			}
			link = fmt.Sprintf("https://docs.google.com/%s/d/%s/edit", app, idHex[:20])
		default:
			link = fmt.Sprintf("https://drive.google.com/file/d/%s/view", idHex[:20])
		}

		rows = append(rows, map[string]any{
			"id":            idHex,
			"name":          sf.name,
			"mime_type":     sf.mime,
			"created_time":  synthRFC3339(createdTS),
			"modified_time": synthRFC3339(modifiedTS),
			"size":          sf.size,
			"web_view_link": link,
			"owner":         owner,
			"shared":        shared,
			"starred":       starred,
			"trashed":       false,
		})
	}
	return rows
}
