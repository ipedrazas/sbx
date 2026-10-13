package clidist

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "pkg/", Typeflag: tar.TypeDir, Mode: 0o755})
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func zipBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	zw.Create("pkg/")
	for name, body := range files {
		w, _ := zw.Create(name)
		w.Write([]byte(body))
	}
	zw.Close()
	return buf.Bytes()
}

func server(t *testing.T, handler http.HandlerFunc) *atomic.Int32 {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	old := releaseBaseURL
	releaseBaseURL = srv.URL
	t.Cleanup(func() { releaseBaseURL = old })
	return &hits
}

func TestEnsureCLIBinaryDownloadsAndCaches(t *testing.T) {
	archive := tarGz(t, map[string]string{"pkg/README.md": "readme", "pkg/dbxcli": "#!/bin/sh\necho hi\n"})
	var gotPath string
	hits := server(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Write(archive)
	})
	dir := t.TempDir()
	p := PlatformByDir("linux-x86_64")

	if _, ok := CachedCLIBinary(dir, "1.2.3", p); ok {
		t.Fatal("cached before download")
	}
	path, err := EnsureCLIBinary(context.Background(), dir, "1.2.3", p)
	if err != nil {
		t.Fatal(err)
	}
	if want := "/tavon-ai/dbxcli/releases/download/v1.2.3/dbxcli-1.2.3-x86_64-unknown-linux-gnu.tar.gz"; gotPath != want {
		t.Fatalf("url path %s", gotPath)
	}
	if want := filepath.Join(dir, "bin", "1.2.3", "linux-x86_64", "dbxcli"); path != want {
		t.Fatalf("path %s", path)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "#!/bin/sh\necho hi\n" {
		t.Fatalf("content %q", b)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(path)
		if fi.Mode().Perm() != 0o755 {
			t.Fatalf("mode %v", fi.Mode())
		}
	}
	again, err := EnsureCLIBinary(context.Background(), dir, "1.2.3", p)
	if err != nil || again != path || hits.Load() != 1 {
		t.Fatalf("second call: %s %v hits=%d", again, err, hits.Load())
	}
	if cached, ok := CachedCLIBinary(dir, "1.2.3", p); !ok || cached != path {
		t.Fatal("not cached")
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("leftover temp files: %v", entries)
	}
}

func TestEnsureCLIBinaryZip(t *testing.T) {
	archive := zipBytes(t, map[string]string{"pkg/dbxcli.exe": "MZ"})
	server(t, func(w http.ResponseWriter, r *http.Request) { w.Write(archive) })
	path, err := EnsureCLIBinary(context.Background(), t.TempDir(), "1.0.0", PlatformByDir("windows-x86_64"))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "MZ" {
		t.Fatal(string(b))
	}
}

func TestEnsureCLIBinaryErrors(t *testing.T) {
	ctx := context.Background()
	p := PlatformByDir("darwin-arm64")

	server(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	_, err := EnsureCLIBinary(ctx, t.TempDir(), "9.9.9", p)
	if err == nil || !strings.Contains(err.Error(), "returned HTTP 404 Not Found") {
		t.Fatal(err)
	}

	cases := map[string][]byte{
		"not a valid tar.gz":                       []byte("garbage"),
		"release archive did not contain `dbxcli`": tarGz(t, map[string]string{"pkg/other": "x"}),
		"not a valid zip":                          []byte("PK\x03\x04garbage"),
	}
	for want, body := range cases {
		server(t, func(w http.ResponseWriter, r *http.Request) { w.Write(body) })
		_, err := EnsureCLIBinary(ctx, t.TempDir(), "1.0.0", p)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want %q, got %v", want, err)
		}
	}
	server(t, func(w http.ResponseWriter, r *http.Request) { w.Write(zipBytes(t, map[string]string{"x": "y"})) })
	if _, err := EnsureCLIBinary(ctx, t.TempDir(), "1.0.0", p); err == nil || !strings.Contains(err.Error(), "did not contain") {
		t.Error(err)
	}

	// Truncated gzip stream.
	full := tarGz(t, map[string]string{"pkg/dbxcli": strings.Repeat("x", 4096)})
	server(t, func(w http.ResponseWriter, r *http.Request) { w.Write(full[:len(full)/2]) })
	if _, err := EnsureCLIBinary(ctx, t.TempDir(), "1.0.0", p); err == nil {
		t.Error("expected error for truncated archive")
	}

	releaseBaseURL = "http://127.0.0.1:1"
	if _, err := EnsureCLIBinary(ctx, t.TempDir(), "1.0.0", p); err == nil || !strings.Contains(err.Error(), "request failed for") {
		t.Error(err)
	}

	// Cache dir cannot be created (data dir is a file).
	f := filepath.Join(t.TempDir(), "file")
	os.WriteFile(f, nil, 0o644)
	if _, err := EnsureCLIBinary(ctx, f, "1.0.0", p); err == nil || !strings.Contains(err.Error(), "failed to create cache dir") {
		t.Error(err)
	}
}

func TestCLIVersion(t *testing.T) {
	t.Setenv("DBX_CLI_VERSION", "")
	if CLIVersion() != defaultDBXCLIVersion || defaultDBXCLIVersion == "" {
		t.Fatal(CLIVersion())
	}
	t.Setenv("DBX_CLI_VERSION", "  v2.0.1 ")
	if CLIVersion() != "2.0.1" {
		t.Fatal(CLIVersion())
	}
	t.Setenv("DBX_CLI_VERSION", "v")
	if CLIVersion() != defaultDBXCLIVersion {
		t.Fatal(CLIVersion())
	}
}

func TestPlatforms(t *testing.T) {
	_ = HostPlatform() // must not panic
	cases := map[string]string{
		"darwin/arm64": "darwin-arm64", "linux/arm64": "linux-aarch64",
		"linux/amd64": "linux-x86_64", "windows/amd64": "windows-x86_64",
	}
	for k, want := range cases {
		parts := strings.Split(k, "/")
		if p := hostPlatform(parts[0], parts[1]); p == nil || p.Dir != want {
			t.Errorf("%s: %v", k, p)
		}
	}
	if hostPlatform("darwin", "amd64") != nil {
		t.Error("intel mac should be unsupported")
	}
	if PlatformByDir("nope") != nil {
		t.Error("unknown dir")
	}
	if p := PlatformByDir("windows-x86_64"); p.Exe != "dbxcli.exe" || p.Triple != "x86_64-pc-windows-msvc" {
		t.Error(p)
	}
}
