package claudehooks

import (
	"context"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/orchael/bridgectl/internal/bridge"
)

// Opt-in acceptance through the real Claude terminal and built hook helper.
// It uses native authentication and asks only a fixed, harmless question.
func TestClaudeRealTUIQuestion(t *testing.T) {
	helper := os.Getenv("BRIDGECTL_HOOK_TEST_BINARY")
	if helper == "" {
		t.Skip("set BRIDGECTL_HOOK_TEST_BINARY to a freshly built bridgectl for authenticated acceptance")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	o, err := New(ctx, helper)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, "claude", "--settings", o.SettingsPath, "--permission-mode", "plan", "Use AskUserQuestion to ask exactly one multiple-choice question: BLUE or GREEN? Do not read files or run commands. After I answer, reply with just that color.")
	cmd.Dir = t.TempDir()
	for _, e := range os.Environ() {
		if len(e) < 11 || e[:11] != "CLAUDECODE=" {
			cmd.Env = append(cmd.Env, e)
		}
	}
	terminal, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 40, Cols: 120})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); _ = terminal.Close(); _ = cmd.Wait() }()
	logPath := os.Getenv("BRIDGECTL_HOOK_TEST_LOG")
	output := io.Discard
	if logPath != "" {
		log, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = log.Close() }()
		output = log
	}
	go func() { _, _ = io.Copy(output, terminal) }()
	// Accept the disposable working directory's trust dialog. No tools are run.
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	answered, working := false, false
	answerAttempts := 0
	for {
		select {
		case <-timer.C:
			if !answered {
				_, _ = terminal.Write([]byte("\x1b[B\r"))
			} else if !working && answerAttempts < 3 {
				_, _ = terminal.Write([]byte("\r"))
				answerAttempts++
				timer.Reset(time.Second)
			}
		case next := <-o.Updates:
			t.Logf("state=%s pending=%t", next.State, next.Pending != nil)
			if next.State == bridge.InteractionWaitingForInput && !answered {
				answered = true
				// Hooks run before the dialog renders; wait before selecting.
				timer.Reset(time.Second)
			}
			if answered && next.State == bridge.InteractionWorking {
				working = true
			}
			if working && next.State == bridge.InteractionIdle {
				return
			}
		case <-ctx.Done():
			t.Fatal("Claude did not complete input-wait, answer, working, idle cycle")
		}
	}
}
