package pii

import (
	"encoding/json"
	"fmt"
	"testing"
)

func classIs(c *Class, want Class) bool { return c != nil && *c == want }

func TestNameHints(t *testing.T) {
	cases := map[string]Class{
		"employee_ssn": DirectIdentifier, "date_of_birth": DirectIdentifier,
		"email_address": Contact, "mobile_phone": Contact,
		"credit_card": Financial, "iban_number": Financial,
		"zip_code": QuasiIdentifier, "ethnicity": QuasiIdentifier,
		"Patient_ID": Health, "customer_name": DirectIdentifier, "API_KEY": DirectIdentifier,
	}
	for name, want := range cases {
		if got := classifyByName(name); !classIs(got, want) {
			t.Errorf("%s: got %v want %s", name, got, want)
		}
	}
}

func TestBareNameNotFlagged(t *testing.T) {
	for _, n := range []string{"table_name", "username"} {
		if got := classifyByName(n); got != nil {
			t.Errorf("%s flagged as %s", n, *got)
		}
	}
}

func strs(ss ...string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func TestEmailValuesDetected(t *testing.T) {
	r := ScanField("contact", "string", strs("a@b.com", "ivan@anthropic.com", "x+y@z.co.uk", "q@r.io"))
	if !classIs(r.PiiClass, Contact) || r.CredentialDetected {
		t.Fatalf("%+v", r)
	}
	if r.SourceName != "contact" {
		t.Fatal(r.SourceName)
	}
}

func TestSSNValuesDetected(t *testing.T) {
	var s []string
	for i := 0; i < 5; i++ {
		s = append(s, fmt.Sprintf("%03d-12-345%d", 100+i, i))
	}
	r := ScanField("id", "string", strs(s...))
	if !classIs(r.PiiClass, DirectIdentifier) {
		t.Fatalf("%+v", r)
	}
}

func TestCreditCardRequiresLuhn(t *testing.T) {
	good := strs("4242424242424242", "4242424242424242", "4242424242424242", "4242424242424242", "4242424242424242")
	if r := ScanField("number", "string", good); !classIs(r.PiiClass, Financial) {
		t.Fatalf("good: %+v", r)
	}
	bad := strs("4242424242424241", "4242424242424241", "4242424242424241", "4242424242424241", "4242424242424241")
	if r := ScanField("number", "string", bad); r.PiiClass != nil && *r.PiiClass != NonPii {
		t.Fatalf("bad: %v", *r.PiiClass)
	}
	// Card-shaped numbers (not strings) are stringified and still match.
	nums := []any{json.Number("4242424242424242"), json.Number("4242424242424242"), json.Number("4242424242424242")}
	if r := ScanField("n", "integer", nums); !classIs(r.PiiClass, Financial) {
		t.Fatalf("numbers: %+v", r)
	}
}

func TestIBANAndPhone(t *testing.T) {
	if r := ScanField("x", "string", strs("GB82WEST12345698765432", "DE89370400440532013000", "FR1420041010050500013M02606")); !classIs(r.PiiClass, Financial) {
		t.Fatalf("iban: %+v", r)
	}
	if r := ScanField("x", "string", strs("+1 (555) 123-4567", "555.123.4567", "+44 20 7946 0958")); !classIs(r.PiiClass, Contact) {
		t.Fatalf("phone: %+v", r)
	}
	// Unicode whitespace inside a phone number (Rust \s is Unicode).
	if r := ScanField("x", "string", strs("555 123 4567", "555 123 4567", "555 123 4567")); !classIs(r.PiiClass, Contact) {
		t.Fatalf("unicode phone: %+v", r)
	}
	// Too many digits for a phone.
	if r := ScanField("x", "string", strs("1234-5678-9012-3456-7", "1234-5678-9012-3456-8", "1234-5678-9012-3456-9")); r.PiiClass != nil {
		t.Fatalf("long digits: %+v", r)
	}
	// Dates alone stay unclassified; name hint upgrades.
	dates := strs("2020-01-01", "2021-02-02", "2022-03-03")
	if r := ScanField("when", "string", dates); r.PiiClass != nil {
		t.Fatalf("dates: %+v", r)
	}
	if r := ScanField("dob", "string", dates); !classIs(r.PiiClass, DirectIdentifier) {
		t.Fatalf("dob: %+v", r)
	}
}

func TestCredentials(t *testing.T) {
	if r := ScanField("token", "string", strs("sk-abcdef1234567890ghijklmnopqrstuv")); !r.CredentialDetected {
		t.Fatal("openai key")
	}
	jwt := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"
	if r := ScanField("auth", "string", strs(jwt, jwt, jwt)); !r.CredentialDetected {
		t.Fatal("jwt")
	}
	if r := ScanField("api_key", "string", strs("internal-rotated-hourly")); !r.CredentialDetected {
		t.Fatal("name-based")
	}
	for _, k := range []string{"AKIAIOSFODNN7EXAMPLE", "ghp_" + "abcdefghijklmnopqrstuvwxyz0123456789", "xoxb-1234567890-abc", "glpat-abcdefghijklmnopqrstu"} {
		if r := ScanField("v", "string", strs(k)); !r.CredentialDetected {
			t.Errorf("%s not detected", k)
		}
	}
	if r := ScanField("v", "string", strs("hello world")); r.CredentialDetected {
		t.Error("false positive")
	}
}

func TestQuorumRejectsOneOffMatch(t *testing.T) {
	var s []any
	for i := 0; i < 9; i++ {
		s = append(s, fmt.Sprintf("text %d", i))
	}
	s = append(s, "stray@example.com", nil, "   ", true)
	if r := ScanField("notes", "string", s); r.PiiClass != nil {
		t.Fatalf("%+v", r)
	}
}

func TestApplyPolicy(t *testing.T) {
	if _, keep := ApplyPolicy("hi", PolicyDrop); keep {
		t.Fatal("drop kept")
	}
	if v, _ := ApplyPolicy("ivan@example.com", PolicyMask); v != "***" {
		t.Fatal(v)
	}
	if v, _ := ApplyPolicy(42.0, PolicyMask); v != "***" {
		t.Fatal(v)
	}
	if v, _ := ApplyPolicy(nil, PolicyMask); v != nil {
		t.Fatal(v)
	}
	if v, keep := ApplyPolicy("x", PolicyNone); v != "x" || !keep {
		t.Fatal(v)
	}
	cases := []struct {
		in, want any
	}{
		{"ivan@example.com", "*@example.com"},
		{"2026-04-22", "2026"},
		{"opaque", "***"},
		{"a@b", "***"},
		{12345, 10000.0},
		{json.Number("12345"), 10000.0},
		{-0.05, -0.01},
		{0.0, int64(0)},
		{nil, nil},
		{true, "***"},
		{map[string]any{"a": 1}, "***"},
	}
	for _, c := range cases {
		got, keep := ApplyPolicy(c.in, PolicyGeneralize)
		if !keep || fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("generalize(%v) = %v (%T), want %v", c.in, got, got, c.want)
		}
	}
}

func TestParsePolicy(t *testing.T) {
	for _, s := range ValidPolicyValues {
		if p, ok := ParsePolicy(s); !ok || string(p) != s {
			t.Errorf("%s rejected", s)
		}
	}
	for _, s := range []string{"redact", "remove", ""} {
		if _, ok := ParsePolicy(s); ok {
			t.Errorf("%q accepted", s)
		}
	}
	if PolicyFromString("redact") != PolicyNone || PolicyFromString("drop") != PolicyDrop {
		t.Fatal("from string")
	}
}

func TestParseClass(t *testing.T) {
	for _, c := range []Class{DirectIdentifier, QuasiIdentifier, Contact, Health, Financial, NonPii} {
		if got, ok := ParseClass(c.String()); !ok || got != c {
			t.Errorf("%s", c)
		}
	}
	if _, ok := ParseClass("bogus"); ok {
		t.Fatal("bogus accepted")
	}
}

func TestLuhnShort(t *testing.T) {
	if luhnValid("4242") {
		t.Fatal("short accepted")
	}
}
