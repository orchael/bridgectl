package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// withAgentRuntimeLock serializes updates across processes. The lock lives
// beside the runtime so it remains stable while node_modules is replaced.
func withAgentRuntimeLock(ctx context.Context, runtimeDir string, update func(string) error) error {
	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	canonical, err := filepath.EvalSymlinks(runtimeDir)
	if err != nil {
		return fmt.Errorf("resolve provider runtime: %w", err)
	}
	path := filepath.Join(filepath.Dir(canonical), "."+filepath.Base(canonical)+".update.lock")
	lock, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open provider runtime lock: %w", err)
	}
	defer func() { _ = lock.Close() }()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		acquired, err := tryAgentRuntimeLock(lock)
		if err != nil {
			return fmt.Errorf("lock provider runtime: %w", err)
		}
		if acquired {
			break
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("wait for provider runtime lock: %w", waitCtx.Err())
		case <-ticker.C:
		}
	}
	defer func() { _ = unlockAgentRuntime(lock) }()
	return update(canonical)
}
