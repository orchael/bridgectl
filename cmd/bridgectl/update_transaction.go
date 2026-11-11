package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var agentRuntimeItems = []string{"package.json", "pnpm-lock.yaml", "node_modules"}

type agentUpdateTransaction struct {
	Backup string          `json:"backup"`
	State  string          `json:"state"`
	Had    map[string]bool `json:"had"`
}

func agentUpdateMarker(runtimeDir string) string {
	return filepath.Join(filepath.Dir(runtimeDir), "."+filepath.Base(runtimeDir)+".update-transaction.json")
}

// recoverAgentRuntime runs while the provider runtime lock is held. The marker
// is written before any live file moves, so a later update can finish rollback.
func recoverAgentRuntime(runtimeDir string) error {
	marker := agentUpdateMarker(runtimeDir)
	data, err := os.ReadFile(marker)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var txn agentUpdateTransaction
	if err := json.Unmarshal(data, &txn); err != nil {
		return fmt.Errorf("read agent update transaction: %w", err)
	}
	parent := filepath.Dir(runtimeDir)
	if filepath.Dir(txn.Backup) != parent || !strings.HasPrefix(filepath.Base(txn.Backup), ".bridgectl-agents-backup-") ||
		(txn.State != "in_progress" && txn.State != "committed" && txn.State != "rolled_back") || txn.Had == nil {
		return fmt.Errorf("invalid agent update transaction at %s", marker)
	}
	info, err := os.Lstat(txn.Backup)
	if errors.Is(err, os.ErrNotExist) && (txn.State == "committed" || txn.State == "rolled_back") {
		return os.Remove(marker)
	}
	if err != nil {
		return fmt.Errorf("agent update backup %s: %w", txn.Backup, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("agent update backup is not a directory: %s", txn.Backup)
	}
	if txn.State == "in_progress" {
		for _, name := range agentRuntimeItems {
			live, old := filepath.Join(runtimeDir, name), filepath.Join(txn.Backup, name)
			_, oldErr := os.Lstat(old)
			if oldErr != nil && !errors.Is(oldErr, os.ErrNotExist) {
				return oldErr
			}
			_, liveErr := os.Lstat(live)
			if liveErr != nil && !errors.Is(liveErr, os.ErrNotExist) {
				return liveErr
			}
			if txn.Had[name] && errors.Is(oldErr, os.ErrNotExist) && errors.Is(liveErr, os.ErrNotExist) {
				return fmt.Errorf("cannot restore %s: both live and backup are missing", name)
			}
			if oldErr == nil || !txn.Had[name] {
				if liveErr == nil {
					if err := os.RemoveAll(live); err != nil {
						return fmt.Errorf("remove staged %s: %w", name, err)
					}
				}
				if oldErr == nil {
					if err := os.Rename(old, live); err != nil {
						return fmt.Errorf("restore %s: %w", name, err)
					}
				}
			}
		}
		txn.State = "rolled_back"
		if err := atomicJSON(marker, txn); err != nil {
			return fmt.Errorf("mark agent update rolled back: %w", err)
		}
	}
	if err := os.RemoveAll(txn.Backup); err != nil {
		return fmt.Errorf("clean up agent update backup: %w", err)
	}
	return os.Remove(marker)
}
