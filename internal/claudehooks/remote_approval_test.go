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
		"long":        permissionInput(strings.Repeat("x", 300)),
		"control":     permissionInput("echo \x1b[31m"),
		"newline":     permissionInput("git add .\ngit push"),
		"tab":         permissionInput("echo\tpush"),
		"cwd newline": `{"session_id":"s","hook_event_name":"PermissionRequest","tool_name":"Bash","cwd":"/repo\nRun once in /x","tool_input":{"command":"ls"}}`,
		"cwd tab":     `{"session_id":"s","hook_event_name":"PermissionRequest","tool_name":"Bash","cwd":"/re\tpo","tool_input":{"command":"ls"}}`,
		"not bash":    `{"session_id":"s","hook_event_name":"PermissionRequest","tool_name":"Write","cwd":"/repo","tool_input":{"file_path":"/repo/a"}}`,
		"no cwd":      `{"session_id":"s","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"ls"}}`,
		"no command":  `{"session_id":"s","hook_event_name":"PermissionRequest","tool_name":"Bash","cwd":"/repo","tool_input":{}}`,
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

// A stale event (another Claude session, or an older prompt) that apply
// rejects must not register a waiter, so it can never take over the current
// request's remote decision channel.
func TestRemoteApprovalIgnoresStaleEvents(t *testing.T) {
	o, cancel := remoteObserver(t)
	defer cancel()
	var current bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- ReportTo(context.Background(), o.ConfigPath, strings.NewReader(permissionInput("git push")), &current)
	}()
	u := nextUpdate(t, o)
	stale := strings.Replace(permissionInput("git push"), `"session_id":"s"`, `"session_id":"other"`, 1)
	var staleOut bytes.Buffer
	ctx, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	if err := ReportTo(ctx, o.ConfigPath, strings.NewReader(stale), &staleOut); err != nil {
		t.Fatal(err)
	}
	if staleOut.Len() != 0 {
		t.Fatalf("stale helper got a decision: %q", staleOut.String())
	}
	if err := o.Decide(u.Pending.ID, "accept"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil || !strings.Contains(current.String(), `"behavior":"allow"`) {
		t.Fatalf("current helper lost its decision: err=%v out=%q", err, current.String())
	}
}

// Two identical concurrent Bash permission requests can correlate to the same
// pending request. The second registration is ambiguous: it must not replace
// the first helper's waiter, and it returns no decision so its local dialog
// handles it.
func TestRemoteApprovalRefusesAmbiguousRegistration(t *testing.T) {
	o, cancel := remoteObserver(t)
	defer cancel()
	var first bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- ReportTo(context.Background(), o.ConfigPath, strings.NewReader(permissionInput("git push")), &first)
	}()
	u := nextUpdate(t, o)
	var second bytes.Buffer
	ctx, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	if err := ReportTo(ctx, o.ConfigPath, strings.NewReader(permissionInput("git push")), &second); err != nil {
		t.Fatal(err)
	}
	if second.Len() != 0 || ctx.Err() != nil {
		t.Fatalf("ambiguous helper must return promptly with no decision: out=%q ctx=%v", second.String(), ctx.Err())
	}
	if err := o.Decide(u.Pending.ID, "accept"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil || !strings.Contains(first.String(), `"behavior":"allow"`) {
		t.Fatalf("first helper lost its decision: err=%v out=%q", err, first.String())
	}
}

// "Approve once": after a decision, the request stays pending until Claude
// reports the tool result. A second correlated request in that window must not
// register under the consumed ID, so retrying the decision can't approve it.
func TestRemoteApprovalIDIsSingleUse(t *testing.T) {
	o, cancel := remoteObserver(t)
	defer cancel()
	var first bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- ReportTo(context.Background(), o.ConfigPath, strings.NewReader(permissionInput("git push")), &first)
	}()
	u := nextUpdate(t, o)
	if err := o.Decide(u.Pending.ID, "accept"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var second bytes.Buffer
	ctx, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	if err := ReportTo(ctx, o.ConfigPath, strings.NewReader(permissionInput("git push")), &second); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != nil || second.Len() != 0 {
		t.Fatalf("second request registered on a consumed ID: out=%q ctx=%v", second.String(), ctx.Err())
	}
	if err := o.Decide(u.Pending.ID, "accept"); !errors.Is(err, bridge.ErrPendingRequestMismatch) {
		t.Fatalf("retried decision approved again: %v", err)
	}
}

// A waiting request that Claude invalidates (here: the turn stops) must stop
// being remotely decidable, and its helper must be released with no decision.
func TestRemoteApprovalInvalidatedRequest(t *testing.T) {
	o, cancel := remoteObserver(t)
	defer cancel()
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- ReportTo(context.Background(), o.ConfigPath, strings.NewReader(permissionInput("git push")), &out)
	}()
	u := nextUpdate(t, o)
	if err := ReportTo(context.Background(), o.ConfigPath, strings.NewReader(`{"session_id":"s","hook_event_name":"Stop"}`), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := o.Decide(u.Pending.ID, "accept"); !errors.Is(err, bridge.ErrPendingRequestMismatch) {
		t.Fatalf("invalidated request still decidable: %v", err)
	}
	select {
	case err := <-done:
		if err != nil || out.Len() != 0 {
			t.Fatalf("released helper: err=%v out=%q", err, out.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("helper not released after its request was invalidated")
	}
}

// Rejection cleanup runs after the handler reacquires the lock. If a new
// prompt already cleared the request and started a turn, the cleanup must not
// mark that newer turn idle.
func TestRejectCleanupDoesNotOverwriteNewTurn(t *testing.T) {
	s := &state{pending: map[string]pending{}, tools: map[string]string{}, waiters: map[string]chan string{}}
	s.pending["/tool/Bash"] = pending{request: bridge.PendingRequest{ID: "old", Type: bridge.PendingRequestApproval}}
	if !s.resolveRejected("old") || s.base != bridge.InteractionIdle {
		t.Fatalf("reject of a live request must resolve it: base=%s", s.base)
	}
	s.base = bridge.InteractionWorking // a new prompt cleared the request first
	if s.resolveRejected("old") || s.base != bridge.InteractionWorking {
		t.Fatalf("stale cleanup changed the new turn: base=%s", s.base)
	}
}
