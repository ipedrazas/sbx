// Package secretstore persists secrets (OAuth tokens, API keys) outside the
// SQLite database: OpenBao/Vault KV v2, the macOS Keychain, or files.
package secretstore

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ErrNotFound is returned when a key has no stored value.
var ErrNotFound = errors.New("secret not found")

// Store is implemented by every secret backend.
type Store interface {
	Store(key string, value []byte) error
	Retrieve(key string) ([]byte, error)
	Delete(key string) error
}

var (
	mu       sync.RWMutex
	instance Store
)

// Init installs the process-wide store. It replaces any previous store, which
// keeps tests simple; the daemon calls it exactly once at startup.
func Init(s Store) {
	mu.Lock()
	instance = s
	mu.Unlock()
}

// IsInitialized reports whether Init has been called.
func IsInitialized() bool {
	mu.RLock()
	defer mu.RUnlock()
	return instance != nil
}

func get() (Store, error) {
	mu.RLock()
	defer mu.RUnlock()
	if instance == nil {
		return nil, errors.New("secret store not initialised")
	}
	return instance, nil
}

// Put stores value under key in the global store.
func Put(key string, value []byte) error {
	s, err := get()
	if err != nil {
		return err
	}
	return s.Store(key, value)
}

// Get retrieves key from the global store.
func Get(key string) ([]byte, error) {
	s, err := get()
	if err != nil {
		return nil, err
	}
	return s.Retrieve(key)
}

// Delete removes key from the global store.
func Delete(key string) error {
	s, err := get()
	if err != nil {
		return err
	}
	return s.Delete(key)
}

const anthropicKeyItem = "anthropic_api_key"

func StoreAnthropicKey(key string) error { return Put(anthropicKeyItem, []byte(key)) }

// GetAnthropicKey returns the stored key, or "" when unset.
func GetAnthropicKey() string {
	b, err := Get(anthropicKeyItem)
	if err != nil {
		return ""
	}
	return string(b)
}

// DeleteAnthropicKey removes the key; a missing key is not an error.
func DeleteAnthropicKey() error {
	if err := Delete(anthropicKeyItem); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	return nil
}

// ── Memory ───────────────────────────────────────────────────────────────────

// Memory is an in-process store, used by tests.
type Memory struct {
	mu sync.Mutex
	m  map[string][]byte
}

func NewMemory() *Memory { return &Memory{m: map[string][]byte{}} }

func (s *Memory) Store(key string, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = bytes.Clone(value)
	return nil
}

func (s *Memory) Retrieve(key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[key]
	if !ok {
		return nil, ErrNotFound
	}
	return bytes.Clone(v), nil
}

func (s *Memory) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[key]; !ok {
		return ErrNotFound
	}
	delete(s.m, key)
	return nil
}

// ── File ─────────────────────────────────────────────────────────────────────

// File stores each secret in its own 0600 file named by the hex of its key.
type File struct{ dir string }

func NewFile(dir string) (*File, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	return &File{dir: dir}, nil
}

func (s *File) path(key string) string { return filepath.Join(s.dir, hex.EncodeToString([]byte(key))) }

func (s *File) Store(key string, value []byte) error {
	if err := os.WriteFile(s.path(key), value, 0o600); err != nil {
		return fmt.Errorf("failed to store secret: %w", err)
	}
	_ = os.Chmod(s.path(key), 0o600)
	return nil
}

func (s *File) Retrieve(key string) ([]byte, error) {
	b, err := os.ReadFile(s.path(key))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve secret: %w", err)
	}
	return b, nil
}

func (s *File) Delete(key string) error {
	err := os.Remove(s.path(key))
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("failed to delete secret: %w", err)
	}
	return nil
}

// ── Vault / OpenBao KV v2 ────────────────────────────────────────────────────

type Vault struct {
	client *http.Client
	addr   string
	token  string
	Mount  string
	Prefix string
}

// NewVault returns a KV v2 store at addr using mount "secret" and prefix "dstudio".
func NewVault(addr, token string) *Vault {
	return &Vault{
		client: &http.Client{Timeout: 5 * time.Second},
		addr:   strings.TrimRight(addr, "/"),
		token:  token,
		Mount:  "secret",
		Prefix: "dstudio",
	}
}

func (s *Vault) url(kind, key string) string {
	return fmt.Sprintf("%s/v1/%s/%s/%s/%s", s.addr, s.Mount, kind, s.Prefix, key)
}

func (s *Vault) do(method, url string, body any) (*http.Response, error) {
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Vault-Token", s.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return s.client.Do(req)
}

func (s *Vault) Store(key string, value []byte) error {
	resp, err := s.do(http.MethodPost, s.url("data", key), map[string]any{
		"data": map[string]any{"value": base64.StdEncoding.EncodeToString(value)},
	})
	if err != nil {
		return fmt.Errorf("failed to store secret: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("failed to store secret: vault returned %s", resp.Status)
	}
	return nil
}

func (s *Vault) Retrieve(key string) ([]byte, error) {
	resp, err := s.do(http.MethodGet, s.url("data", key), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve secret: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("failed to retrieve secret: vault returned %s", resp.Status)
	}
	var body struct {
		Data struct {
			Data struct {
				Value *string `json:"value"`
			} `json:"data"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("failed to retrieve secret: %w", err)
	}
	if body.Data.Data.Value == nil {
		return nil, errors.New("failed to retrieve secret: missing value in vault response")
	}
	return base64.StdEncoding.DecodeString(*body.Data.Data.Value)
}

func (s *Vault) Delete(key string) error {
	resp, err := s.do(http.MethodDelete, s.url("metadata", key), nil)
	if err != nil {
		return fmt.Errorf("failed to delete secret: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("failed to delete secret: vault returned %s", resp.Status)
	}
	return nil
}
