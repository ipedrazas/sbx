package util

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestURLEncode(t *testing.T) {
	if got := URLEncode("hello world"); got != "hello+world" {
		t.Fatalf("got %q", got)
	}
	if got := URLEncode("a+b=c&d"); got != "a%2Bb%3Dc%26d" {
		t.Fatalf("got %q", got)
	}
}

func TestValueAsString(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{"hi", "hi"},
		{float64(42), "42"},
		{1.5, "1.5"},
		{json.Number("12345678901234567890"), "12345678901234567890"},
		{true, "true"},
		{nil, ""},
		{int64(7), "7"},
		{[]any{"a", float64(1)}, `["a",1]`},
		{map[string]any{"b": 1, "a": "<x>"}, `{"a":"<x>","b":1}`},
	}
	for _, c := range cases {
		if got := ValueAsString(c.in); got != c.want {
			t.Errorf("ValueAsString(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestHashToken(t *testing.T) {
	h := HashToken("secret")
	if len(h) != 64 || h != HashToken("secret") || h == HashToken("other") {
		t.Fatalf("bad hash %q", h)
	}
}

func TestBearerToken(t *testing.T) {
	tok := GenerateBearerToken()
	if !strings.HasPrefix(tok, "ds_") || len(tok) != 51 {
		t.Fatalf("bad token %q", tok)
	}
	if tok == GenerateBearerToken() {
		t.Fatal("tokens should differ")
	}
}

func TestPKCE(t *testing.T) {
	v, c := GeneratePKCEPair()
	if len(v) != 64 {
		t.Fatalf("verifier len %d", len(v))
	}
	for _, r := range v {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("-._~", r)
		if !ok {
			t.Fatalf("bad verifier char %q", r)
		}
	}
	sum := sha256.Sum256([]byte(v))
	if c != base64.RawURLEncoding.EncodeToString(sum[:]) || strings.Contains(c, "=") {
		t.Fatalf("bad challenge %q", c)
	}
}

func TestCanonicalHash(t *testing.T) {
	a := map[string]any{"a": 1, "b": 2}
	b := map[string]any{"b": 2, "a": 1}
	c := map[string]any{"a": 1, "b": 3}
	if HashCanonicalJSON(a) != HashCanonicalJSON(b) {
		t.Fatal("key order should not matter")
	}
	if HashCanonicalJSON(a) == HashCanonicalJSON(c) {
		t.Fatal("different values should differ")
	}
}

func TestState(t *testing.T) {
	s := GenerateState()
	if len(s) != 64 || s == GenerateState() {
		t.Fatalf("bad state %q", s)
	}
}

func TestExcerpt(t *testing.T) {
	if got := Excerpt("héllo", 2); got != "hé" {
		t.Fatalf("got %q", got)
	}
	if got := Excerpt("abc", 10); got != "abc" {
		t.Fatalf("got %q", got)
	}
	if got := Excerpt("abc", 0); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestParseJSONMap(t *testing.T) {
	if m := ParseJSONMap(""); len(m) != 0 {
		t.Fatal("expected empty")
	}
	if m := ParseJSONMap("[1]"); len(m) != 0 {
		t.Fatal("expected empty for array")
	}
	m := ParseJSONMap(`{"n": 9007199254740993}`)
	if ValueAsString(m["n"]) != "9007199254740993" {
		t.Fatalf("lost precision: %v", m["n"])
	}
}
