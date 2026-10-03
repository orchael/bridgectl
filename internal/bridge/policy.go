package bridge

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Policy defines runtime limits and guards for the bridge.
type Policy struct {
	MaxPerProject int
	MaxGlobal     int
	MaxInputBytes int
	AllowedPaths  []string // glob patterns for allowed repo_path values
}

// DefaultPolicy returns sensible defaults.
func DefaultPolicy() Policy {
	return Policy{
		MaxPerProject: 5,
		MaxGlobal:     20,
		MaxInputBytes: 65536,
	}
}

// ValidateRepoPath checks that the given path is under one of the allowed path patterns.
// If no patterns are configured, all paths are allowed.
func (p *Policy) ValidateRepoPath(repoPath string) error {
	if len(p.AllowedPaths) == 0 {
		return nil
	}
	abs, err := filepath.Abs(repoPath)
	if err != nil {
		return fmt.Errorf("%w: resolve path: %v", ErrInvalidArgument, err)
	}
	for _, pattern := range p.AllowedPaths {
		matched, err := filepath.Match(pattern, abs)
		if err != nil {
			continue
		}
		if matched {
			return nil
		}
		// Also check if abs is under the pattern's directory tree, respecting
		// path-component boundaries: pattern "/home/mark" must match
		// "/home/mark/repo" but not the sibling "/home/mark-other", which a
		// bare strings.HasPrefix would incorrectly allow.
		patternDir := strings.TrimRight(pattern, "*?")
		patternDir = strings.TrimSuffix(patternDir, string(filepath.Separator))
		if patternDir != "" && (abs == patternDir || strings.HasPrefix(abs, patternDir+string(filepath.Separator))) {
			return nil
		}
	}
	return fmt.Errorf("%w: repo_path %q is not under any allowed path", ErrInvalidArgument, repoPath)
}

// ValidateInput checks that input text does not exceed the maximum size.
func (p *Policy) ValidateInput(text string) error {
	if p.MaxInputBytes > 0 && len(text) > p.MaxInputBytes {
		return fmt.Errorf("%w: input size %d exceeds max %d bytes", ErrInputTooLarge, len(text), p.MaxInputBytes)
	}
	return nil
}

// ValidateInputBytes checks that PTY input bytes do not exceed the maximum size.
func (p *Policy) ValidateInputBytes(data []byte) error {
	if p.MaxInputBytes > 0 && len(data) > p.MaxInputBytes {
		return fmt.Errorf("%w: input size %d exceeds max %d bytes", ErrInputTooLarge, len(data), p.MaxInputBytes)
	}
	return nil
}

// CheckSessionLimits verifies that creating a new session would not exceed limits.
func (p *Policy) CheckSessionLimits(projectCount, globalCount int) error {
	if p.MaxPerProject > 0 && projectCount >= p.MaxPerProject {
		return fmt.Errorf("%w: project limit (%d/%d)", ErrSessionLimitReached, projectCount, p.MaxPerProject)
	}
	if p.MaxGlobal > 0 && globalCount >= p.MaxGlobal {
		return fmt.Errorf("%w: global limit (%d/%d)", ErrSessionLimitReached, globalCount, p.MaxGlobal)
	}
	return nil
}
