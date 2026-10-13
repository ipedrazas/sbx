// Package version carries build provenance stamped in via -ldflags:
//
//	go build -ldflags "-X github.com/ipedrazas/sbx/internal/version.Version=0.1.10 \
//	  -X github.com/ipedrazas/sbx/internal/version.GitSHA=$(git rev-parse --short=12 HEAD) \
//	  -X github.com/ipedrazas/sbx/internal/version.BuildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
package version

import (
	"fmt"
	"runtime/debug"
)

var (
	Version   = "0.1.10"
	GitSHA    = "unknown"
	BuildTime = "unknown"
)

func init() {
	if GitSHA != "unknown" {
		return
	}
	// Fall back to VCS info embedded by `go build` in a git checkout.
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if len(s.Value) > 12 {
				GitSHA = s.Value[:12]
			} else if s.Value != "" {
				GitSHA = s.Value
			}
		case "vcs.time":
			if BuildTime == "unknown" {
				BuildTime = s.Value
			}
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if dirty && GitSHA != "unknown" {
		GitSHA += "-dirty"
	}
}

// Long renders "0.1.10 (git abc, built 2026-...)".
func Long() string {
	return fmt.Sprintf("%s (git %s, built %s)", Version, GitSHA, BuildTime)
}
