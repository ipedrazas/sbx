// Package util holds small helpers shared across the daemon.
package util

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net/url"
	"strconv"
	"strings"
)

// Excerpt cuts s to at most maxChars runes, never splitting a UTF-8 sequence.
func Excerpt(s string, maxChars int) string {
	n := 0
	for i := range s {
		if n == maxChars {
			return s[:i]
		}
		n++
	}
	return s
}

// URLEncode form-encodes s (spaces become '+'), matching url::form_urlencoded.
func URLEncode(s string) string {
	return url.QueryEscape(s)
}

// ValueAsString renders a decoded JSON value as a flat string for filter
// comparisons: strings verbatim, numbers/bools in canonical form, null as "",
// arrays/objects as compact JSON.
func ValueAsString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case json.Number:
		return t.String()
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(t), 'f', -1, 32)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case int32:
		return strconv.FormatInt(int64(t), 10)
	case uint64:
		return strconv.FormatUint(t, 10)
	default:
		b, err := MarshalJSON(v)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// MarshalJSON encodes v as compact JSON without HTML escaping (serde_json
// never escapes '<', '>' or '&'). Map keys come out sorted.
func MarshalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// JSONString is MarshalJSON returning a string, with "null" on error.
func JSONString(v any) string {
	b, err := MarshalJSON(v)
	if err != nil {
		return "null"
	}
	return string(b)
}

// HashToken returns the hex SHA-256 of a bearer token.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// HashCanonicalJSON returns the hex SHA-256 of the canonical (sorted-key,
// compact) JSON rendering of v.
func HashCanonicalJSON(v any) string {
	sum := sha256.Sum256([]byte(JSONString(v)))
	return hex.EncodeToString(sum[:])
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return b
}

// GenerateBearerToken returns a fresh token of the form `ds_<48 hex chars>`.
func GenerateBearerToken() string {
	return "ds_" + hex.EncodeToString(randomBytes(24))
}

// GeneratePKCEPair returns a 64-char code_verifier and its base64url-unpadded
// SHA-256 code_challenge (RFC 7636).
func GeneratePKCEPair() (verifier, challenge string) {
	const charset = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"
	var sb strings.Builder
	max := big.NewInt(int64(len(charset)))
	for range 64 {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic("crypto/rand failed: " + err.Error())
		}
		sb.WriteByte(charset[n.Int64()])
	}
	verifier = sb.String()
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge
}

// GenerateState returns a random 64-hex-char OAuth state parameter.
func GenerateState() string {
	return hex.EncodeToString(randomBytes(32))
}

// DecodeJSON decodes b into a generic value, keeping numbers as json.Number
// so large integers survive round-trips.
func DecodeJSON(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// ParseJSONMap decodes a JSON object string, returning an empty map for
// empty input, invalid JSON or non-object values.
func ParseJSONMap(s string) map[string]any {
	if strings.TrimSpace(s) == "" {
		return map[string]any{}
	}
	v, err := DecodeJSON([]byte(s))
	if err != nil {
		return map[string]any{}
	}
	m, ok := v.(map[string]any)
	if !ok || m == nil {
		return map[string]any{}
	}
	return m
}
