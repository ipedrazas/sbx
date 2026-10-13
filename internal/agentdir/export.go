package agentdir

import (
	"archive/zip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"

	"github.com/ipedrazas/sbx/internal/actions"
	"github.com/ipedrazas/sbx/internal/clidist"
	"github.com/ipedrazas/sbx/internal/crud"
)

// ExportOptions are the options for ExportSkillPackage.
type ExportOptions struct {
	IncludeMCP bool
	Zip        bool
}

// ensureCLIBinary is swappable in tests.
var ensureCLIBinary = clidist.EnsureCLIBinary

// ExportSkillPackage creates a portable, self-contained skill package at
// `<outputDir>/<slug>/`:
//
//	<slug>/
//	├── config.json
//	├── SKILL.md
//	└── scripts/<platform>/dbxcli[.exe]
//
// dbxcli binaries are fetched (once, then cached under `<dataDir>/bin/`) for
// every supported platform. When opts.Zip is set a `<slug>.zip` is created
// alongside the folder. Returns the package dir and the zip path (nil if none).
func ExportSkillPackage(ctx context.Context, db *sql.DB, dataDir string, agentID int64, tokenPlaintext *string,
	daemonURL, outputDir string, opts *ExportOptions) (string, *string, error) {
	agent, err := crud.GetAgent(ctx, db, agentID)
	if err != nil {
		return "", nil, err
	}
	if agent == nil {
		return "", nil, errors.New("agent not found")
	}

	slug := AgentSlug(agent.Name)
	pkgDir := filepath.Join(outputDir, slug)
	scriptsDir := filepath.Join(pkgDir, "scripts")
	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		return "", nil, fmt.Errorf("failed to create package dir: %v", err)
	}

	bindings, err := crud.ListAgentDatasets(ctx, db, agentID)
	if err != nil {
		return "", nil, err
	}
	actionBindings, err := actions.ListAgentActions(ctx, db, agentID)
	if err != nil {
		return "", nil, err
	}

	// config.json
	config, err := BuildConfig(ctx, db, agent, bindings, actionBindings, tokenPlaintext, daemonURL)
	if err != nil {
		return "", nil, err
	}
	configStr, err := prettyJSON(config)
	if err != nil {
		return "", nil, err
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "config.json"), []byte(configStr), 0o644); err != nil {
		return "", nil, fmt.Errorf("failed to write config.json: %v", err)
	}

	// SKILL.md (portable format)
	skill, err := BuildPortableSkillMD(ctx, db, agent, bindings, actionBindings, opts.IncludeMCP)
	if err != nil {
		return "", nil, err
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "SKILL.md"), []byte(skill), 0o644); err != nil {
		return "", nil, fmt.Errorf("failed to write SKILL.md: %v", err)
	}

	// scripts/<platform>/dbxcli — every supported platform; for the host,
	// fall back to a locally-built/installed binary when the download fails.
	version := clidist.CLIVersion()
	hostDir := ""
	if hp := clidist.HostPlatform(); hp != nil {
		hostDir = hp.Dir
	}
	for i := range clidist.Platforms {
		p := &clidist.Platforms[i]
		src, err := ensureCLIBinary(ctx, dataDir, version, p)
		switch {
		case err == nil:
			copyIntoScripts(scriptsDir, p.Dir, p.Exe, src)
		case hostDir != "" && hostDir == p.Dir:
			slog.Warn(fmt.Sprintf("dbxcli download failed (%v); using local build for %s", err, p.Dir))
			copyLocalInto(filepath.Join(scriptsDir, p.Dir), "dbxcli")
		default:
			slog.Warn(fmt.Sprintf("skipping dbxcli for %s: %v", p.Dir, err))
		}
	}

	// scripts/<platform>/dstudio-mcp (optional), from local artifacts.
	if opts.IncludeMCP {
		host := hostDir
		if host == "" {
			host = rustOS() + "-" + rustArch()
		}
		copyLocalInto(filepath.Join(scriptsDir, host), "dstudio-mcp")
		for _, pt := range [][2]string{
			{"linux-aarch64", "aarch64-unknown-linux-gnu"},
			{"linux-x86_64", "x86_64-unknown-linux-gnu"},
		} {
			copyCrossBinary(scriptsDir, pt[0], pt[1], "dstudio-mcp")
		}
	}

	// Optional ZIP
	var zipPath *string
	if opts.Zip {
		zipFile := filepath.Join(outputDir, slug+".zip")
		_ = os.Remove(zipFile)
		if err := createZipFromDir(pkgDir, zipFile); err != nil {
			return "", nil, err
		}
		zipPath = &zipFile
	}
	return pkgDir, zipPath, nil
}

// rustOS / rustArch mirror std::env::consts::{OS,ARCH} naming.
func rustOS() string {
	if runtime.GOOS == "darwin" {
		return "macos"
	}
	return runtime.GOOS
}

func rustArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	case "386":
		return "x86"
	}
	return runtime.GOARCH
}

// createZipFromDir zips srcDir (entries prefixed with its base name, files
// only, Deflate, preserving unix permissions) into zipFile.
func createZipFromDir(srcDir, zipFile string) error {
	root := filepath.Dir(srcDir)
	f, err := os.Create(zipFile)
	if err != nil {
		return fmt.Errorf("failed to create zip file: %v", err)
	}
	archive := zip.NewWriter(f)

	addFile := func(path string, info os.FileInfo) error {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		hdr := &zip.FileHeader{Name: filepath.ToSlash(rel), Method: zip.Deflate}
		hdr.SetMode(info.Mode().Perm())
		hdr.Modified = info.ModTime()
		w, err := archive.CreateHeader(hdr)
		if err != nil {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		_, err = io.Copy(w, in)
		return err
	}

	dirs := []string{srcDir}
	for len(dirs) > 0 {
		dir := dirs[len(dirs)-1]
		dirs = dirs[:len(dirs)-1]
		entries, err := os.ReadDir(dir)
		if err != nil {
			f.Close()
			return err
		}
		for _, e := range entries {
			path := filepath.Join(dir, e.Name())
			info, err := os.Stat(path)
			if err != nil {
				f.Close()
				return err
			}
			if info.IsDir() {
				dirs = append(dirs, path)
				continue
			}
			if err := addFile(path, info); err != nil {
				f.Close()
				return err
			}
		}
	}

	if err := archive.Close(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
