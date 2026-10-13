// Package synthetic generates deterministic fake rows for a dataset's fields,
// seeded per (row, column) so repeated calls return identical data.
package synthetic

import (
	"fmt"
	"strings"

	"github.com/ipedrazas/sbx/internal/models"
)

var firstNames = []string{
	"Alice", "Bob", "Carol", "David", "Eve", "Frank", "Grace", "Hank", "Ivy", "Jack", "Karen",
	"Leo", "Maya", "Noah", "Olivia", "Paul", "Quinn", "Rita", "Sam", "Tina", "Uma", "Victor",
	"Wendy", "Xavier", "Yara", "Zach",
}

var lastNames = []string{
	"Smith", "Johnson", "Williams", "Brown", "Jones", "Garcia", "Miller", "Davis", "Rodriguez",
	"Martinez", "Hernandez", "Lopez", "Gonzalez", "Wilson", "Anderson", "Thomas", "Taylor",
	"Moore", "Jackson", "Martin",
}

var domains = []string{"example.com", "test.org", "demo.net", "sample.io", "fake.dev"}

var words = []string{
	"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel", "india", "juliet",
	"kilo", "lima", "mike", "november", "oscar", "papa", "quebec", "romeo", "sierra", "tango",
}

var cities = []string{
	"London", "Paris", "Tokyo", "New York", "Berlin", "Sydney", "Toronto", "Madrid", "Rome", "Amsterdam",
}

var countries = []string{
	"United States", "United Kingdom", "Canada", "Australia", "Germany", "France", "Japan",
	"Spain", "Italy", "Netherlands",
}

var statuses = []string{"active", "inactive", "pending", "archived"}

func seededPick(items []string, seed uint64) string {
	return items[seed%uint64(len(items))]
}

func seededHash(row int64, col string) uint64 {
	var h rustHasher
	h.writeI64(row)
	h.writeStr(col)
	return h.finish()
}

type fieldHint int

const (
	hintID fieldHint = iota
	hintEmail
	hintFirstName
	hintLastName
	hintName
	hintPhone
	hintCity
	hintCountry
	hintAddress
	hintStatus
	hintURL
	hintText
	hintDateTime
	hintCurrency
	hintSmallInt
	hintBoolean
	hintGeneric
)

func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

func classifyFieldName(name string) fieldHint {
	lower := strings.ToLower(name)
	switch {
	case lower == "id" || strings.HasSuffix(lower, "_id") || strings.HasSuffix(lower, "id"):
		return hintID
	case strings.Contains(lower, "email"):
		return hintEmail
	case containsAny(lower, "first_name", "firstname", "given_name"):
		return hintFirstName
	case containsAny(lower, "last_name", "lastname", "surname", "family_name"):
		return hintLastName
	case containsAny(lower, "name", "title", "subject"):
		return hintName
	case containsAny(lower, "phone", "mobile", "tel"):
		return hintPhone
	case containsAny(lower, "city", "town"):
		return hintCity
	case strings.Contains(lower, "country"):
		return hintCountry
	case containsAny(lower, "address", "street"):
		return hintAddress
	case containsAny(lower, "status", "state"):
		return hintStatus
	case containsAny(lower, "url", "link", "website"):
		return hintURL
	case containsAny(lower, "description", "bio", "summary", "body", "content"):
		return hintText
	case containsAny(lower, "created", "updated", "date", "_at", "timestamp"):
		return hintDateTime
	case containsAny(lower, "amount", "price", "cost", "total"):
		return hintCurrency
	case containsAny(lower, "count", "quantity", "age", "score"):
		return hintSmallInt
	case containsAny(lower, "active", "enabled", "verified", "is_"):
		return hintBoolean
	}
	return hintGeneric
}

func generateValue(field *models.DatasetField, rowIdx int64) any {
	seed := seededHash(rowIdx, field.SourceName)
	hint := classifyFieldName(field.SourceName)
	ft := strings.ToLower(field.FieldType)

	if ft == "boolean" || hint == hintBoolean {
		return seed%2 == 0
	}

	switch hint {
	case hintID:
		return fmt.Sprintf("%d", rowIdx+1)
	case hintEmail:
		first := strings.ToLower(seededPick(firstNames, seed))
		last := strings.ToLower(seededPick(lastNames, seed>>8))
		domain := seededPick(domains, seed>>16)
		return fmt.Sprintf("%s.%s@%s", first, last, domain)
	case hintFirstName:
		return seededPick(firstNames, seed)
	case hintLastName:
		return seededPick(lastNames, seed)
	case hintName:
		return seededPick(firstNames, seed) + " " + seededPick(lastNames, seed>>8)
	case hintPhone:
		area := 200 + seed%800
		mid := (seed >> 10) % 1000
		end := (seed >> 20) % 10000
		return fmt.Sprintf("+1-%d-%03d-%04d", area, mid, end)
	case hintCity:
		return seededPick(cities, seed)
	case hintCountry:
		return seededPick(countries, seed)
	case hintAddress:
		num := 1 + seed%999
		return fmt.Sprintf("%d %s Street", num, seededPick(words, seed>>8))
	case hintStatus:
		return seededPick(statuses, seed)
	case hintURL:
		return fmt.Sprintf("https://%s/%s", seededPick(domains, seed), seededPick(words, seed>>8))
	case hintText:
		n := 5 + seed%10
		ws := make([]string, 0, n)
		for i := uint64(0); i < n; i++ {
			ws = append(ws, seededPick(words, seed+i))
		}
		s := strings.Join(ws, " ")
		if len(s) > 0 {
			s = strings.ToUpper(s[:1]) + s[1:]
		}
		return s + "."
	case hintDateTime:
		day := 1 + seed%28
		month := 1 + (seed>>5)%12
		year := 2023 + (seed>>9)%3
		hour := seed % 24
		minute := (seed >> 5) % 60
		return fmt.Sprintf("%04d-%02d-%02dT%02d:%02d:00Z", year, month, day, hour, minute)
	case hintCurrency:
		if ft == "integer" {
			return int64(seed % 10000)
		}
		whole := float64(seed % 10000)
		cents := (seed >> 16) % 100
		return whole + float64(cents)/100.0
	case hintSmallInt:
		return int64(seed % 1000)
	}

	// Generic
	switch ft {
	case "integer":
		return int64(seed % 10000)
	case "float":
		return float64(seed%10000) / 100.0
	case "datetime":
		day := 1 + seed%28
		month := 1 + (seed>>5)%12
		return fmt.Sprintf("2024-%02d-%02dT12:00:00Z", month, day)
	case "json":
		return map[string]any{"key": seededPick(words, seed)}
	}
	return fmt.Sprintf("%s_%s_%d", seededPick(words, seed), seededPick(words, seed>>8), rowIdx+1)
}

// GenerateRows builds count synthetic rows keyed by each visible field's
// display name. An "id" column is added when no visible field is named id.
func GenerateRows(fields []models.DatasetField, count int64) []map[string]any {
	var visible []*models.DatasetField
	for i := range fields {
		if fields[i].Visible {
			visible = append(visible, &fields[i])
		}
	}
	if len(visible) == 0 {
		return []map[string]any{}
	}
	hasID := false
	for _, f := range visible {
		if strings.ToLower(f.SourceName) == "id" {
			hasID = true
			break
		}
	}
	rows := make([]map[string]any, 0, max(count, 0))
	for rowIdx := int64(0); rowIdx < count; rowIdx++ {
		obj := make(map[string]any, len(visible)+1)
		if !hasID {
			obj["id"] = fmt.Sprintf("%d", rowIdx+1)
		}
		for _, f := range visible {
			obj[f.DisplayName] = generateValue(f, rowIdx)
		}
		rows = append(rows, obj)
	}
	return rows
}
