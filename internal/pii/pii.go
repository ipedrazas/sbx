// Package pii detects PII and credentials in sampled field values and applies
// per-field redaction policies at serve time.
//
// Detection runs two cheap passes: a substring match on the column name
// against curated hints, and format regexes over sampled values (requiring a
// 40% quorum of non-null samples, except credentials which flag on one hit).
// There is no LLM pass in v1.
package pii

import (
	"encoding/json"
	"math"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/ipedrazas/sbx/internal/util"
)

// matchQuorum is the minimum fraction of non-null samples that must match a
// regex before the field is flagged as that class.
const matchQuorum float32 = 0.4

// minSamplesForRegex is the minimum non-null samples required before regex
// classification runs; below it only the name heuristic applies.
const minSamplesForRegex = 3

// Class is a PII classification.
type Class string

const (
	DirectIdentifier Class = "direct_identifier"
	QuasiIdentifier  Class = "quasi_identifier"
	Contact          Class = "contact"
	Health           Class = "health"
	Financial        Class = "financial"
	NonPii           Class = "non_pii"
)

// String returns the wire form of c.
func (c Class) String() string { return string(c) }

// ParseClass parses a wire value; ok is false for unknown values.
func ParseClass(s string) (Class, bool) {
	switch c := Class(s); c {
	case DirectIdentifier, QuasiIdentifier, Contact, Health, Financial, NonPii:
		return c, true
	}
	return "", false
}

// Policy is a field-level redaction policy.
type Policy string

const (
	PolicyNone       Policy = "none"
	PolicyMask       Policy = "mask"
	PolicyDrop       Policy = "drop"
	PolicyGeneralize Policy = "generalize"
)

// ValidPolicyValues lists every accepted wire value for `pii_policy`.
var ValidPolicyValues = [4]string{"none", "mask", "drop", "generalize"}

// ParsePolicy is the strict write-path parse: ok is false for unknown values so
// callers can reject them (a typo must not silently serve PII in the clear).
func ParsePolicy(s string) (Policy, bool) {
	switch p := Policy(s); p {
	case PolicyNone, PolicyMask, PolicyDrop, PolicyGeneralize:
		return p, true
	}
	return "", false
}

// PolicyFromString is the lenient serve-path parse: unknown values become
// PolicyNone (legacy rows written before validation existed).
func PolicyFromString(s string) Policy {
	if p, ok := ParsePolicy(s); ok {
		return p
	}
	return PolicyNone
}

// FieldScanResult is the outcome of ScanField.
type FieldScanResult struct {
	SourceName         string
	PiiClass           *Class
	CredentialDetected bool
}

// Rust's regex crate treats \d and \s as Unicode classes; mirror that here.
const (
	uDigit = `\p{Nd}`
	uSpace = `\s\x{0B}\x{85}\p{Z}`
)

var (
	reEmail   = regexp.MustCompile(`^[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}$`)
	rePhone   = regexp.MustCompile(`^\+?[0-9][` + uDigit + uSpace + `().\-]{6,}` + uDigit + `$`)
	reSSNUS   = regexp.MustCompile(`^` + uDigit + `{3}-` + uDigit + `{2}-` + uDigit + `{4}$`)
	reCC      = regexp.MustCompile(`^` + uDigit + `{13,19}$`)
	reIBAN    = regexp.MustCompile(`^[A-Z]{2}` + uDigit + `{2}[A-Z0-9]{11,30}$`)
	reDateISO = regexp.MustCompile(`^` + uDigit + `{4}-` + uDigit + `{2}-` + uDigit + `{2}$`)

	// Credentials: high-signal prefixes, then JWTs (the loosest pattern).
	reCredPrefix = regexp.MustCompile(`^(` +
		`sk-[A-Za-z0-9_\-]{20,}` + // OpenAI / Anthropic-style
		`|sk-ant-[A-Za-z0-9_\-]{20,}` + // Anthropic explicitly
		`|ghp_[A-Za-z0-9]{30,}` + // GitHub personal access token
		`|ghs_[A-Za-z0-9]{30,}` + // GitHub server token
		`|gho_[A-Za-z0-9]{30,}` + // GitHub OAuth token
		`|github_pat_[A-Za-z0-9_]{22,}` +
		`|AKIA[0-9A-Z]{16}` + // AWS access key
		`|ASIA[0-9A-Z]{16}` + // AWS temporary credentials
		`|AIza[0-9A-Za-z_\-]{35}` + // Google API key
		`|xox[abprs]-[A-Za-z0-9\-]{10,}` + // Slack tokens
		`|glpat-[A-Za-z0-9_\-]{20,}` + // GitLab PAT
		`)$`)
	reJWT = regexp.MustCompile(`^eyJ[A-Za-z0-9_\-]{10,}\.eyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}$`)
)

type nameHint struct {
	substrings []string
	class      Class
}

// nameHints: first hit wins, so more specific matches come first.
var nameHints = []nameHint{
	// Credentials (the real signal is credential_detected; the class just
	// keeps the field from showing as unclassified).
	{[]string{"api_key", "api-key", "apikey", "access_token", "access-token", "secret_key", "secret-key",
		"client_secret", "private_key", "bearer_token", "bearer-token", "password", "passwd"}, DirectIdentifier},
	{[]string{"ssn", "social_security", "national_id", "nin", "passport", "driver_license",
		"drivers_license", "dl_number"}, DirectIdentifier},
	{[]string{"dob", "date_of_birth", "birth_date", "birthday"}, DirectIdentifier},
	{[]string{"full_name", "first_name", "last_name", "given_name", "family_name", "surname",
		"middle_name"}, DirectIdentifier},
	{[]string{"credit_card", "card_number", "cc_number", "cvv", "cvc", "iban", "swift", "bic",
		"account_number", "routing_number"}, Financial},
	{[]string{"email", "e_mail", "e-mail"}, Contact},
	{[]string{"phone", "mobile", "telephone", "tel_no", "fax"}, Contact},
	{[]string{"diagnosis", "icd10", "icd_10", "medication", "prescription", "patient_id",
		"medical_record", "mrn", "health_condition"}, Health},
	// Quasi-identifiers: re-identifying in combination.
	{[]string{"address", "street", "zip", "zipcode", "postal_code", "city", "state", "province",
		"country"}, QuasiIdentifier},
	{[]string{"gender", "ethnicity", "race", "nationality", "occupation"}, QuasiIdentifier},
	// Bare "name" is too noisy (username, table_name); only explicit person names.
	{[]string{"person_name", "customer_name", "contact_name"}, DirectIdentifier},
}

func classifyByName(sourceName string) *Class {
	lower := strings.ToLower(sourceName)
	for _, h := range nameHints {
		for _, n := range h.substrings {
			if strings.Contains(lower, n) {
				c := h.class
				return &c
			}
		}
	}
	return nil
}

type valueTally struct {
	nonNull, email, phone, ssn, cc, iban, date, credential int
}

func tallyValues(samples []any) valueTally {
	var t valueTally
	for _, v := range samples {
		switch s := v.(type) {
		case nil:
			continue
		case string:
			t.add(s)
		default:
			// Numbers/bools/objects: stringify so a card-shaped integer still matches.
			t.add(util.JSONString(v))
		}
	}
	return t
}

func (t *valueTally) add(s string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return
	}
	t.nonNull++
	// Credentials first: they're the stronger signal.
	if reCredPrefix.MatchString(s) || reJWT.MatchString(s) {
		t.credential++
		return
	}
	switch {
	case reEmail.MatchString(s):
		t.email++
	case reSSNUS.MatchString(s):
		t.ssn++
	case reIBAN.MatchString(s):
		t.iban++
	case reCC.MatchString(s) && luhnValid(s):
		t.cc++
	case reDateISO.MatchString(s):
		t.date++
	case rePhone.MatchString(s):
		// Phones have 7–15 digits (E.164 max); 16+ is a card or account.
		digits := 0
		for i := 0; i < len(s); i++ {
			if s[i] >= '0' && s[i] <= '9' {
				digits++
			}
		}
		if digits >= 7 && digits <= 15 {
			t.phone++
		}
	}
}

func luhnValid(s string) bool {
	var digits []int
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			digits = append(digits, int(s[i]-'0'))
		}
	}
	if len(digits) < 13 {
		return false
	}
	sum := 0
	for i := 0; i < len(digits); i++ {
		d := digits[len(digits)-1-i]
		if i%2 == 1 {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
	}
	return sum%10 == 0
}

func classifyByValues(t valueTally) (*Class, bool) {
	// Credentials bypass the sample-count gate.
	credential := t.credential > 0
	if t.nonNull < minSamplesForRegex {
		return nil, credential
	}
	quorum := int(math.Ceil(float64(float32(t.nonNull) * matchQuorum)))
	var c Class
	switch {
	case t.ssn >= quorum:
		c = DirectIdentifier
	case t.cc >= quorum || t.iban >= quorum:
		c = Financial
	case t.email >= quorum || t.phone >= quorum:
		c = Contact
	default:
		// Dates alone stay non-PII; a DOB name hint upgrades them.
		return nil, credential
	}
	return &c, credential
}

// ScanField classifies a field from its name and a sample of values. Value
// evidence wins over the name hint; credentials flag on either signal.
func ScanField(sourceName, _ string, samples []any) FieldScanResult {
	nameClass := classifyByName(sourceName)
	valueClass, valueCred := classifyByValues(tallyValues(samples))
	class := valueClass
	if class == nil {
		class = nameClass
	}
	return FieldScanResult{
		SourceName:         sourceName,
		PiiClass:           class,
		CredentialDetected: valueCred || isCredentialName(sourceName),
	}
}

var credentialNeedles = []string{
	"api_key", "api-key", "apikey", "access_token", "access-token", "bearer_token", "bearer-token",
	"secret_key", "secret-key", "client_secret", "private_key", "password", "passwd", "auth_token",
}

func isCredentialName(sourceName string) bool {
	lower := strings.ToLower(sourceName)
	for _, n := range credentialNeedles {
		if strings.Contains(lower, n) {
			return true
		}
	}
	return false
}

// ApplyPolicy transforms a single JSON value. keep is false for PolicyDrop —
// callers must remove the key. Mask replaces with "***"; Generalize widens
// dates to the year, emails to the domain, numbers to a power-of-ten bucket,
// and masks anything else.
func ApplyPolicy(value any, policy Policy) (out any, keep bool) {
	switch policy {
	case PolicyDrop:
		return nil, false
	case PolicyMask:
		return maskValue(value), true
	case PolicyGeneralize:
		return generalizeValue(value), true
	}
	return value, true
}

func maskValue(value any) any {
	if value == nil {
		return nil
	}
	return "***"
}

func generalizeValue(value any) any {
	switch v := value.(type) {
	case nil:
		return nil
	case string:
		if reDateISO.MatchString(v) {
			return firstRunes(v, 4)
		}
		if at := strings.IndexByte(v, '@'); at >= 0 && reEmail.MatchString(v) {
			return "*@" + v[at+1:]
		}
		return "***"
	}
	f, ok := asFloat(value)
	if !ok {
		return maskValue(value)
	}
	if f == 0 {
		return int64(0)
	}
	sign := 1.0
	if f < 0 {
		sign = -1.0
	}
	rounded := sign * math.Pow(10, math.Floor(math.Log10(math.Abs(f))))
	if math.IsNaN(rounded) || math.IsInf(rounded, 0) {
		return "***"
	}
	return rounded
}

func firstRunes(s string, n int) string {
	i := 0
	for j := 0; j < n && i < len(s); j++ {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return s[:i]
}

func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}
