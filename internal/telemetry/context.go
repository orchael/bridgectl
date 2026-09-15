package telemetry

import (
	"bufio"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const identityKeyBytes = 32

// SessionContext is bounded, privacy-safe metadata captured once per session.
type SessionContext struct {
	ActorID            string `json:"actor_id,omitempty"`
	SourceLabel        string `json:"source_label,omitempty"`
	OS                 string `json:"os"`
	Arch               string `json:"arch"`
	MachineID          string `json:"machine_id,omitempty"`
	WorkingDirectoryID string `json:"working_directory_id,omitempty"`
	RepositoryID       string `json:"repository_id,omitempty"`
	Branch             string `json:"branch,omitempty"`
	CommitSHA          string `json:"commit_sha,omitempty"`
}

// LoadOrCreateIdentityKey loads a private 32-byte HMAC key or atomically
// creates one so concurrent first starts converge on the same value.
func LoadOrCreateIdentityKey(path string) ([]byte, error) {
	if key, err := readIdentityKey(path); err == nil {
		return key, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create telemetry identity directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("secure telemetry identity directory: %w", err)
	}
	key := make([]byte, identityKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate telemetry identity key: %w", err)
	}
	temp, err := os.CreateTemp(dir, ".identity-key-*")
	if err != nil {
		return nil, fmt.Errorf("create telemetry identity key: %w", err)
	}
	tempPath := temp.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if err := temp.Chmod(0o600); err != nil {
		return nil, errors.Join(err, temp.Close())
	}
	if _, err := temp.Write(key); err != nil {
		return nil, errors.Join(err, temp.Close())
	}
	if err := temp.Sync(); err != nil {
		return nil, errors.Join(err, temp.Close())
	}
	if err := temp.Close(); err != nil {
		return nil, err
	}
	if err := os.Link(tempPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return readIdentityKey(path)
		}
		return nil, fmt.Errorf("persist telemetry identity key: %w", err)
	}
	if err := syncDirectory(dir); err != nil {
		return nil, fmt.Errorf("sync telemetry identity directory: %w", err)
	}
	return key, nil
}

func readIdentityKey(path string) ([]byte, error) {
	cleanPath := filepath.Clean(path)
	info, err := os.Lstat(cleanPath)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("telemetry identity key %q must be a regular file", path)
	}
	key, err := os.ReadFile(cleanPath)
	if err != nil {
		return nil, err
	}
	if len(key) != identityKeyBytes {
		return nil, fmt.Errorf("telemetry identity key %q must contain exactly %d bytes", path, identityKeyBytes)
	}
	if err := os.Chmod(cleanPath, 0o600); err != nil {
		return nil, fmt.Errorf("secure telemetry identity key: %w", err)
	}
	return key, nil
}

// DiscoverSessionContext derives stable keyed identifiers without returning raw
// paths or repository URLs.
func DiscoverSessionContext(repoPath, actorID, sourceLabel string, key []byte) SessionContext {
	context := SessionContext{ActorID: actorID, SourceLabel: sourceLabel, OS: runtime.GOOS, Arch: runtime.GOARCH}
	if len(key) != identityKeyBytes {
		return context
	}
	context.MachineID = keyedID(key, "machine")
	if repoPath == "" {
		return context
	}
	absPath, err := filepath.Abs(repoPath)
	if err != nil {
		return context
	}
	absPath = filepath.Clean(absPath)
	context.WorkingDirectoryID = keyedID(key, absPath)
	repoRoot, gitDir := findGitDirectory(absPath)
	if gitDir == "" {
		return context
	}
	commonDir := gitCommonDirectory(gitDir)
	remote := readOriginRemote(filepath.Join(commonDir, "config"))
	canonical := canonicalRemote(remote)
	if canonical != "" {
		context.RepositoryID = keyedID(key, canonical)
	} else {
		if commonDir != gitDir {
			repoRoot = commonDir
			if filepath.Base(commonDir) == ".git" {
				repoRoot = filepath.Dir(commonDir)
			}
		}
		context.RepositoryID = keyedID(key, repoRoot)
	}
	context.Branch, context.CommitSHA = readGitHead(gitDir, commonDir)
	return context
}

// Git keeps shared config and refs in commondir, but HEAD is worktree-local.
// Missing or unusable metadata leaves discovery best-effort and local.
func gitCommonDirectory(gitDir string) string {
	data, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
	if err != nil {
		return gitDir
	}
	commonDir := strings.TrimSpace(string(data))
	if commonDir == "" || strings.ContainsAny(commonDir, "\r\n\x00") {
		return gitDir
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(gitDir, commonDir)
	}
	commonDir = filepath.Clean(commonDir)
	if info, err := os.Stat(commonDir); err != nil || !info.IsDir() {
		return gitDir
	}
	return commonDir
}

func keyedID(key []byte, value string) string {
	digest := hmac.New(sha256.New, key)
	_, _ = digest.Write([]byte(value))
	return hex.EncodeToString(digest.Sum(nil))
}

func findGitDirectory(start string) (string, string) {
	current := start
	for {
		candidate := filepath.Join(current, ".git")
		if info, err := os.Stat(candidate); err == nil {
			if info.IsDir() {
				return current, candidate
			}
			data, readErr := os.ReadFile(candidate)
			if readErr == nil && strings.HasPrefix(strings.TrimSpace(string(data)), "gitdir:") {
				gitDir := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(data)), "gitdir:"))
				if !filepath.IsAbs(gitDir) {
					gitDir = filepath.Join(current, gitDir)
				}
				return current, filepath.Clean(gitDir)
			}
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", ""
		}
		current = parent
	}
}

func readOriginRemote(path string) string {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	inOrigin := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "[") {
			inOrigin = strings.EqualFold(line, `[remote "origin"]`)
			continue
		}
		if inOrigin {
			key, value, found := strings.Cut(line, "=")
			if found && strings.EqualFold(strings.TrimSpace(key), "url") {
				return strings.TrimSpace(value)
			}
		}
	}
	return ""
}

func canonicalRemote(remote string) string {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return ""
	}
	if parsed, err := url.Parse(remote); err == nil && parsed.Scheme != "" && parsed.Hostname() != "" {
		host := strings.ToLower(parsed.Hostname())
		path := strings.TrimSuffix(strings.TrimSuffix(parsed.EscapedPath(), "/"), ".git")
		return strings.ToLower(parsed.Scheme) + "://" + host + path
	}
	colon := strings.IndexByte(remote, ':')
	if colon > 0 && !strings.Contains(remote[:colon], "/") {
		host := remote[:colon]
		if at := strings.LastIndexByte(host, '@'); at >= 0 {
			host = host[at+1:]
		}
		host = strings.ToLower(host)
		path := strings.TrimSuffix(strings.TrimSuffix(remote[colon+1:], "/"), ".git")
		return "ssh://" + host + "/" + path
	}
	return ""
}

func readGitHead(gitDir, commonDir string) (string, string) {
	data, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return "", ""
	}
	head := strings.TrimSpace(string(data))
	if !strings.HasPrefix(head, "ref:") {
		return "", head
	}
	ref := strings.TrimSpace(strings.TrimPrefix(head, "ref:"))
	branch := strings.TrimPrefix(ref, "refs/heads/")
	commit, err := os.ReadFile(filepath.Join(gitDir, filepath.FromSlash(ref)))
	if err != nil && commonDir != gitDir {
		commit, _ = os.ReadFile(filepath.Join(commonDir, filepath.FromSlash(ref)))
	}
	return branch, strings.TrimSpace(string(commit))
}
