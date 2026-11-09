package claudehooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/orchael/bridgectl/internal/bridge"
)

// Opt-in remote approval (usability review, 2026-11-09): a Bash permission
// request blocks the hook helper until Bridge decides, the local dialog stays
// live (Claude Code shows it concurrently and kills the hook if answered
// locally), and the first answer wins.

func remoteObserver(t *testing.T) (*Observer, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	o, err := NewWithOptions(ctx, "/bin/bridgectl", Options{RemoteApproval: true})
	if err != nil {
		t.Fatal(err)
	}
	return o, cancel
}

func permissionInput(command string) string {
	b, _ := json.Marshal(map[string]any{"session_id": "s", "hook_event_name": "PermissionRequest", "tool_name": "Bash", "cwd": "/repo", "tool_input": map[string]any{"command": command, "description": "push"}})
	return string(b)
}

func nextUpdate(t *testing.T, o *Observer) bridge.Interaction {
	t.Helper()
	select {
	case u := <-o.Updates:
		return u
	case <-time.After(2 * time.Second):
		t.Fatal("no update")
	}
	return bridge.Interaction{}
}

func TestRemoteApprovalSettings(t *testing.T) {
	o, cancel := remoteObserver(t)
	defer cancel()
	data, _ := os.ReadFile(o.SettingsPath)
	var settings struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Timeout int `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	if got := settings.Hooks["PermissionRequest"][0].Hooks[0].Timeout; got != remoteHookTimeoutSeconds {
		t.Fatalf("PermissionRequest timeout %d", got)
	}
	if got := settings.Hooks["PreToolUse"][0].Hooks[0].Timeout; got != 2 {
		t.Fatalf("other hooks must stay short: %d", got)
	}
}

func TestRemoteApprovalDecisions(t *testing.T) {
	for _, tc := range []struct{ decision, behavior string }{{"accept", "allow"}, {"cancel", "deny"}} {
		t.Run(tc.decision, func(t *testing.T) {
			o, cancel := remoteObserver(t)
			defer cancel()
			var out bytes.Buffer
			done := make(chan error, 1)
			go func() {
				done <- ReportTo(context.Background(), o.ConfigPath, strings.NewReader(permissionInput("git push origin main")), &out)
			}()
			u := nextUpdate(t, o)
			if u.State != bridge.InteractionWaitingForApproval || u.Pending == nil || !u.Evidence.Capability.StructuredApprovalSupported {
				t.Fatalf("not actionable: %+v", u)
			}
			if u.Pending.Summary != "Run once in /repo: git push origin main" {
				t.Fatalf("summary %q", u.Pending.Summary)
			}
			if err := o.Decide(u.Pending.ID, tc.decision); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			var reply struct {
				Output struct {
					Event    string `json:"hookEventName"`
					Decision struct {
						Behavior    string          `json:"behavior"`
						Interrupt   bool            `json:"interrupt"`
						Permissions json.RawMessage `json:"updatedPermissions"`
					} `json:"decision"`
				} `json:"hookSpecificOutput"`
			}
			if err := json.Unmarshal(out.Bytes(), &reply); err != nil {
				t.Fatalf("hook output %q: %v", out.String(), err)
			}
			if reply.Output.Event != "PermissionRequest" || reply.Output.Decision.Behavior != tc.behavior || reply.Output.Decision.Permissions != nil {
				t.Fatalf("hook output %s", out.String())
			}
			if tc.decision == "cancel" && !reply.Output.Decision.Interrupt {
				t.Fatal("reject must stop the turn, matching Codex cancel")
			}
			if err := o.Decide(u.Pending.ID, tc.decision); !errors.Is(err, bridge.ErrPendingRequestMismatch) {
				t.Fatalf("second decision: %v", err)
			}
			after := nextUpdate(t, o)
			if after.Evidence.Capability.StructuredApprovalSupported {
				t.Fatal("capability still advertised after the decision")
			}
			// A remote reject interrupts the turn, and Claude fires no Stop or
			// PostToolUse hook for it, so the observer must clear the request
			// itself instead of showing "waiting for approval" forever.
			if tc.decision == "cancel" && (after.State != bridge.InteractionIdle || after.Pending != nil) {
				t.Fatalf("after remote reject: %+v", after)
			}
			if tc.decision == "accept" && after.State != bridge.InteractionWaitingForApproval {
				t.Fatalf("accept should leave the request until Claude reports the tool result: %+v", after)
			}
		})
	}
}

// The person answered in the terminal: Claude Code kills the hook, which the
// observer sees as the waiter's request ending. A late remote decision is refused.
func TestRemoteApprovalLocalAnswerWins(t *testing.T) {
	o, cancel := remoteObserver(t)
	defer cancel()
	hookCtx, killHook := context.WithCancel(context.Background())
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- ReportTo(hookCtx, o.ConfigPath, strings.NewReader(permissionInput("git push")), &out)
	}()
	u := nextUpdate(t, o)
	killHook()
	<-done
	if after := nextUpdate(t, o); after.Evidence.Capability.StructuredApprovalSupported {
		t.Fatal("capability survived the local answer")
	}
	if err := o.Decide(u.Pending.ID, "accept"); !errors.Is(err, bridge.ErrPendingRequestMismatch) {
		t.Fatalf("late remote decision: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("hook wrote a decision after the local answer: %q", out.String())
	}
}

func TestRemoteApprovalNotActionable(t *testing.T) {
	for name, input := range map[string]string{
		"long":       permissionInput(strings.Repeat("x", 300)),
		"control":    permissionInput("echo \x1b[31m"),
		"not bash":   `{"session_id":"s","hook_event_name":"PermissionRequest","tool_name":"Write","cwd":"/repo","tool_input":{"file_path":"/repo/a"}}`,
		"no cwd":     `{"session_id":"s","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"ls"}}`,
		"no command": `{"session_id":"s","hook_event_name":"PermissionRequest","tool_name":"Bash","cwd":"/repo","tool_input":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			o, cancel := remoteObserver(t)
			defer cancel()
			var out bytes.Buffer
			ctx, stop := context.WithTimeout(context.Background(), 2*time.Second)
			defer stop()
			if err := ReportTo(ctx, o.ConfigPath, strings.NewReader(input), &out); err != nil {
				t.Fatal(err)
			}
			u := nextUpdate(t, o)
			if u.State != bridge.InteractionWaitingForApproval || u.Evidence.Capability.StructuredApprovalSupported || out.Len() != 0 {
				t.Fatalf("must observe only: %+v out=%q", u, out.String())
			}
			if u.Pending == nil || u.Pending.Summary != "" {
				t.Fatalf("observe-only requests carry no summary: %+v", u.Pending)
			}
		})
	}
}

// Without the opt-in, nothing changes: no waiting, no summary, no capability.
func TestRemoteApprovalOffByDefault(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o, err := New(ctx, "/bin/bridgectl")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := ReportTo(ctx, o.ConfigPath, strings.NewReader(permissionInput("git push")), &out); err != nil {
		t.Fatal(err)
	}
	u := nextUpdate(t, o)
	if u.Evidence.Capability.StructuredApprovalSupported || u.Pending == nil || u.Pending.Summary != "" || out.Len() != 0 {
		t.Fatalf("default mode changed: %+v", u)
	}
	if err := o.Decide(u.Pending.ID, "accept"); !errors.Is(err, bridge.ErrPendingRequestMismatch) {
		t.Fatalf("default mode accepted a decision: %v", err)
	}
}
