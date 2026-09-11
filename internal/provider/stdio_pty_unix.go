//go:build !windows

package provider

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/creack/pty"
)

func (p *StdioProvider) validateStartupPrompt(ctx context.Context, env []string) error {
	if p.promptRe == nil {
		return nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, p.cfg.StartupTimeout)
	defer cancel()

	binPath, err := resolveBinaryPath(p.cfg.Binary, p.cfg.ProviderRoot)
	if err != nil {
		return err
	}
	args, err := commandArgsForProvider(p.cfg.ProviderID, p.cfg.DefaultArgs, p.cfg.ProviderRoot)
	if err != nil {
		return err
	}
	wd, _ := os.Getwd()
	cmd := exec.CommandContext(probeCtx, binPath, args...)
	cmd.Dir = wd
	cmd.Env = env

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 120, Rows: 40})
	if err != nil {
		return fmt.Errorf("provider %q startup probe: %w", p.cfg.ProviderID, err)
	}
	defer func() {
		_ = ptmx.Close()
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		}
	}()

	buf := make([]byte, 4096)
	var seen bytes.Buffer
	for {
		if probeCtx.Err() != nil {
			return fmt.Errorf("provider %q startup probe timed out waiting for prompt %q; output:\n%s", p.cfg.ProviderID, p.cfg.PromptPattern, seen.String())
		}
		_ = ptmx.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, readErr := ptmx.Read(buf)
		if n > 0 {
			seen.Write(buf[:n])
			if p.promptRe.Match(seen.Bytes()) {
				return nil
			}
		}
		if readErr != nil {
			if os.IsTimeout(readErr) {
				continue
			}
			return fmt.Errorf("provider %q startup probe failed: %v; output:\n%s", p.cfg.ProviderID, readErr, seen.String())
		}
	}
}

func (p *StdioProvider) validateStartupOutput(ctx context.Context, env []string) error {
	probeCtx, cancel := context.WithTimeout(ctx, p.cfg.StartupTimeout)
	defer cancel()

	binPath, err := resolveBinaryPath(p.cfg.Binary, p.cfg.ProviderRoot)
	if err != nil {
		return err
	}
	args, err := commandArgsForProvider(p.cfg.ProviderID, p.cfg.DefaultArgs, p.cfg.ProviderRoot)
	if err != nil {
		return err
	}
	wd, _ := os.Getwd()
	cmd := exec.CommandContext(probeCtx, binPath, args...)
	cmd.Dir = wd
	cmd.Env = env

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 120, Rows: 40})
	if err != nil {
		return fmt.Errorf("provider %q startup probe: %w", p.cfg.ProviderID, err)
	}
	defer func() {
		_ = ptmx.Close()
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		}
	}()

	buf := make([]byte, 4096)
	var seen bytes.Buffer
	for {
		if probeCtx.Err() != nil {
			return fmt.Errorf("provider %q startup probe timed out waiting for output; output:\n%s", p.cfg.ProviderID, seen.String())
		}
		_ = ptmx.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, readErr := ptmx.Read(buf)
		if n > 0 {
			seen.Write(buf[:n])
			slog.Debug("provider startup probe output", "provider", p.cfg.ProviderID, "bytes", n)
			time.Sleep(250 * time.Millisecond)
			return nil
		}
		if readErr != nil {
			if os.IsTimeout(readErr) {
				continue
			}
			return fmt.Errorf("provider %q startup probe failed: %v; output:\n%s", p.cfg.ProviderID, readErr, seen.String())
		}
	}
}
