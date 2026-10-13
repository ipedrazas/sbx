package secretstore

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// KeychainService is the generic-password service name shared with the Rust daemon.
const KeychainService = "ai.tavon.dstudio"

// Keychain stores secrets in the macOS login keychain via the `security` CLI,
// which avoids cgo. Values are written hex-encoded (-X) so arbitrary bytes
// survive, and read back with -w.
type Keychain struct{}

// KeychainAvailable reports whether the macOS `security` tool can be used.
func KeychainAvailable() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	_, err := exec.LookPath("security")
	return err == nil
}

// errSecItemNotFound exit status from `security`.
const secNotFoundExit = 44

func (Keychain) Store(key string, value []byte) error {
	_ = exec.Command("security", "delete-generic-password", "-s", KeychainService, "-a", key).Run()
	out, err := exec.Command("security", "add-generic-password", "-U", "-s", KeychainService, "-a", key,
		"-X", hex.EncodeToString(value)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to store secret: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func (Keychain) Retrieve(key string) ([]byte, error) {
	out, err := exec.Command("security", "find-generic-password", "-s", KeychainService, "-a", key, "-w").Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == secNotFoundExit {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("failed to retrieve secret: %w", err)
	}
	return decodeSecurityOutput(strings.TrimRight(string(out), "\n")), nil
}

func (Keychain) Delete(key string) error {
	err := exec.Command("security", "delete-generic-password", "-s", KeychainService, "-a", key).Run()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == secNotFoundExit {
			return ErrNotFound
		}
		return fmt.Errorf("failed to delete secret: %w", err)
	}
	return nil
}

// decodeSecurityOutput handles `security -w` output, which is the raw text
// for printable values but hex when the value has non-printable bytes. A hex
// string that decodes to printable bytes must have been printed as text.
func decodeSecurityOutput(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) == 0 {
		return []byte(s)
	}
	for _, c := range b {
		if c > 0x7e || (c < 0x20 && c != '\n' && c != '\t' && c != '\r') {
			return b
		}
	}
	return []byte(s)
}
