package secretstore

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func exercise(t *testing.T, s Store) {
	t.Helper()
	if _, err := s.Retrieve("k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want not found, got %v", err)
	}
	if err := s.Store("k", []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if err := s.Store("k", []byte("v2")); err != nil {
		t.Fatal(err)
	}
	got, err := s.Retrieve("k")
	if err != nil || string(got) != "v2" {
		t.Fatalf("got %q %v", got, err)
	}
	if err := s.Delete("k"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want not found, got %v", err)
	}
}

func TestMemory(t *testing.T) { exercise(t, NewMemory()) }

func TestFile(t *testing.T) {
	s, err := NewFile(t.TempDir() + "/secrets")
	if err != nil {
		t.Fatal(err)
	}
	exercise(t, s)
}

// fakeVault implements just enough of the KV v2 API.
func fakeVault(t *testing.T) *httptest.Server {
	var mu sync.Mutex
	data := map[string]string{}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Vault-Token") != "tok" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		key := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		switch {
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/v1/secret/data/dstudio/"):
			var body struct {
				Data struct {
					Value string `json:"value"`
				} `json:"data"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			data[key] = body.Data.Value
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet:
			v, ok := data[key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": map[string]any{"value": v}}})
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/metadata/"):
			if _, ok := data[key]; !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			delete(data, key)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
}

func TestVault(t *testing.T) {
	srv := fakeVault(t)
	defer srv.Close()
	exercise(t, NewVault(srv.URL+"/", "tok"))

	bad := NewVault(srv.URL, "wrong")
	if err := bad.Store("k", []byte("v")); err == nil {
		t.Fatal("expected error with bad token")
	}
	if _, err := bad.Retrieve("k"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("expected non-notfound error, got %v", err)
	}
}

func TestGlobalAndAnthropicHelpers(t *testing.T) {
	Init(nil)
	if IsInitialized() {
		t.Fatal("should be uninitialised")
	}
	if err := Put("x", nil); err == nil {
		t.Fatal("expected error before init")
	}
	Init(NewMemory())
	if !IsInitialized() {
		t.Fatal("should be initialised")
	}
	if GetAnthropicKey() != "" {
		t.Fatal("expected empty key")
	}
	if err := StoreAnthropicKey("sk-ant"); err != nil {
		t.Fatal(err)
	}
	if GetAnthropicKey() != "sk-ant" {
		t.Fatal("key not stored")
	}
	if err := DeleteAnthropicKey(); err != nil {
		t.Fatal(err)
	}
	if err := DeleteAnthropicKey(); err != nil {
		t.Fatal("deleting a missing key should succeed")
	}
}

func TestDecodeSecurityOutput(t *testing.T) {
	if got := string(decodeSecurityOutput("plain-text")); got != "plain-text" {
		t.Fatalf("got %q", got)
	}
	// "deadbeef" decodes to non-printables → hex output from security.
	if got := decodeSecurityOutput("00ff"); len(got) != 2 || got[1] != 0xff {
		t.Fatalf("got %v", got)
	}
	// "4142" decodes to "AB" (printable) → security would have printed "AB",
	// so "4142" itself is the literal value.
	if got := string(decodeSecurityOutput("4142")); got != "4142" {
		t.Fatalf("got %q", got)
	}
	_ = base64.StdEncoding
}
