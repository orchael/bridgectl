package telemetry

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

var sourceIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ValidSourceID reports whether sourceID is safe to use as a telemetry identity.
func ValidSourceID(sourceID string) bool { return sourceIDPattern.MatchString(sourceID) }

// ResolveSourceID returns a configured source ID or securely persists a
// generated UUID. Linking a complete temporary file makes concurrent first
// starts converge on one identity without exposing a partial file.
func ResolveSourceID(configured, path string) (string, error) {
	configured = strings.TrimSpace(configured)
	if configured != "" {
		if !ValidSourceID(configured) {
			return "", fmt.Errorf("source ID must start with an alphanumeric character and contain only alphanumerics, '.', '_', or '-'")
		}
		return configured, nil
	}
	if sourceID, err := readSourceID(path); err == nil {
		return sourceID, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create source ID directory: %w", err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("secure source ID directory: %w", err)
	}
	sourceID := uuid.NewString()
	temp, err := os.CreateTemp(filepath.Dir(path), ".source-id-*")
	if err != nil {
		return "", fmt.Errorf("create source ID: %w", err)
	}
	tempPath := temp.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if err := temp.Chmod(0o600); err != nil {
		return "", errors.Join(err, temp.Close())
	}
	if _, err := temp.WriteString(sourceID + "\n"); err != nil {
		return "", errors.Join(err, temp.Close())
	}
	if err := temp.Sync(); err != nil {
		return "", errors.Join(err, temp.Close())
	}
	if err := temp.Close(); err != nil {
		return "", err
	}
	if err := os.Link(tempPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return readSourceID(path)
		}
		return "", fmt.Errorf("persist source ID: %w", err)
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return "", fmt.Errorf("sync source ID directory: %w", err)
	}
	return sourceID, nil
}

func readSourceID(path string) (string, error) {
	cleanPath := filepath.Clean(path)
	info, err := os.Lstat(cleanPath)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("telemetry source ID %q must be a regular file", path)
	}
	data, err := os.ReadFile(cleanPath)
	if err != nil {
		return "", err
	}
	sourceID := strings.TrimSpace(string(data))
	if !ValidSourceID(sourceID) {
		return "", fmt.Errorf("invalid persisted telemetry source ID in %q", path)
	}
	if err := os.Chmod(cleanPath, 0o600); err != nil {
		return "", fmt.Errorf("secure telemetry source ID: %w", err)
	}
	return sourceID, nil
}
