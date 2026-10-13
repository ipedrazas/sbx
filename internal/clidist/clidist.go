// Package clidist distributes the `dbxcli` binary. The CLI ships per-platform
// release archives from its public repo; the daemon fetches them lazily on
// first export and caches them under `<data_dir>/bin/<version>/<platform>/`.
package clidist

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

// releaseRepo publishes `dbxcli` releases (public, no auth needed).
const releaseRepo = "tavon-ai/dbxcli"

// defaultDBXCLIVersion is the release the daemon was built and tested against.
// Override at runtime with DBX_CLI_VERSION (bare semver, no leading `v`).
const defaultDBXCLIVersion = "0.1.9"

// Overridable in tests.
var (
	releaseBaseURL = "https://github.com"
	httpClient     = &http.Client{}
)

// Platform is a target we ship a `dbxcli` binary for.
type Platform struct {
	// Dir is the directory name in the export package (`scripts/<dir>/`).
	Dir string
	// Triple is the Rust target triple used in the release asset name.
	Triple string
	// Exe is the executable file name on this platform.
	Exe string
}

// Platforms is every platform bundled into an export; must match the
// platforms published in the release named by CLIVersion (a missing asset
// just 404s and is skipped).
var Platforms = []Platform{
	{Dir: "darwin-arm64", Triple: "aarch64-apple-darwin", Exe: "dbxcli"},
	{Dir: "linux-aarch64", Triple: "aarch64-unknown-linux-gnu", Exe: "dbxcli"},
	{Dir: "linux-x86_64", Triple: "x86_64-unknown-linux-gnu", Exe: "dbxcli"},
	{Dir: "windows-x86_64", Triple: "x86_64-pc-windows-msvc", Exe: "dbxcli.exe"},
}

// CLIVersion returns DBX_CLI_VERSION (trimmed, leading `v` stripped) when set
// and non-empty, else the compiled-in default.
func CLIVersion() string {
	v := strings.TrimLeft(strings.TrimSpace(os.Getenv("DBX_CLI_VERSION")), "v")
	if v == "" {
		return defaultDBXCLIVersion
	}
	return v
}

// HostPlatform returns the platform matching the running host, or nil.
func HostPlatform() *Platform {
	return hostPlatform(runtime.GOOS, runtime.GOARCH)
}

func hostPlatform(goos, goarch string) *Platform {
	var dir string
	switch goos + "/" + goarch {
	case "darwin/arm64":
		dir = "darwin-arm64"
	case "linux/arm64":
		dir = "linux-aarch64"
	case "linux/amd64":
		dir = "linux-x86_64"
	case "windows/amd64":
		dir = "windows-x86_64"
	default:
		return nil
	}
	return PlatformByDir(dir)
}

// PlatformByDir looks up a platform by package directory name (e.g. "linux-x86_64").
func PlatformByDir(dir string) *Platform {
	for i := range Platforms {
		if Platforms[i].Dir == dir {
			return &Platforms[i]
		}
	}
	return nil
}

func cacheDir(dataDir, version string) string {
	return filepath.Join(dataDir, "bin", version)
}

// CachedCLIBinary returns the path of an already-cached binary; ok is false
// when it hasn't been fetched. Never downloads.
func CachedCLIBinary(dataDir, version string, p *Platform) (string, bool) {
	dest := filepath.Join(cacheDir(dataDir, version), p.Dir, p.Exe)
	if fi, err := os.Stat(dest); err == nil && fi.Mode().IsRegular() {
		return dest, true
	}
	return "", false
}

// EnsureCLIBinary makes sure the binary for p is cached, downloading it from
// the public release on a miss, and returns the cached executable's path.
func EnsureCLIBinary(ctx context.Context, dataDir, version string, p *Platform) (string, error) {
	destDir := filepath.Join(cacheDir(dataDir, version), p.Dir)
	dest := filepath.Join(destDir, p.Exe)
	if _, err := os.Stat(dest); err == nil {
		return dest, nil
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", fmt.Errorf("failed to create cache dir %s: %v", destDir, err)
	}

	url := fmt.Sprintf("%s/%s/releases/download/v%s/dbxcli-%s-%s.tar.gz",
		releaseBaseURL, releaseRepo, version, version, p.Triple)
	slog.Info(fmt.Sprintf("fetching dbxcli %s for %s from %s", version, p.Dir, url))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("request failed for %s: %v", url, err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed for %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("download %s returned HTTP %d %s", url, resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read body from %s: %v", url, err)
	}
	if err := extractExe(body, p.Exe, dest); err != nil {
		return "", err
	}
	return dest, nil
}

// extractExe pulls exeName out of a .tar.gz (or .zip) archive and writes it to
// dest with 0755 perms.
func extractExe(data []byte, exeName, dest string) error {
	if bytes.HasPrefix(data, []byte("PK\x03\x04")) {
		return extractFromZip(data, exeName, dest)
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("not a valid tar.gz: %v", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("bad tar entry: %v", err)
		}
		if hdr.Typeflag != tar.TypeReg || path.Base(hdr.Name) != exeName {
			continue
		}
		buf, err := io.ReadAll(tr)
		if err != nil {
			return fmt.Errorf("failed to read %s from archive: %v", exeName, err)
		}
		return writeExe(dest, buf)
	}
	return fmt.Errorf("release archive did not contain `%s`", exeName)
}

func extractFromZip(data []byte, exeName, dest string) error {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fmt.Errorf("not a valid zip: %v", err)
	}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || path.Base(f.Name) != exeName {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("failed to read %s from archive: %v", exeName, err)
		}
		buf, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return fmt.Errorf("failed to read %s from archive: %v", exeName, err)
		}
		return writeExe(dest, buf)
	}
	return fmt.Errorf("release archive did not contain `%s`", exeName)
}

// writeExe writes via a temp file + rename so a concurrent reader never sees a
// partially written binary.
func writeExe(dest string, buf []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(dest), "."+filepath.Base(dest)+".*")
	if err != nil {
		return fmt.Errorf("failed to write %s: %v", dest, err)
	}
	tmpName := tmp.Name()
	_, werr := tmp.Write(buf)
	cerr := tmp.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		_ = os.Chmod(tmpName, 0o755)
		werr = os.Rename(tmpName, dest)
	}
	if werr != nil {
		os.Remove(tmpName)
		return fmt.Errorf("failed to write %s: %v", dest, werr)
	}
	return nil
}
