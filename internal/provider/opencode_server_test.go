package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/orchael/bridgectl/internal/bridge"
)

func TestNewOpenCodeServerProvider_Defaults(t *testing.T) {
	p := NewOpenCodeServerProvider(OpenCodeServerConfig{
		ProviderID: "opencode-server",
		Binary:     "/bin/echo",
	})

	if p.ID() != "opencode-server" {
		t.Fatalf("ID() = %q, want %q", p.ID(), "opencode-server")
	}
	if p.Binary() != "/bin/echo" {
		t.Fatalf("Binary() = %q, want %q", p.Binary(), "/bin/echo")
	}
	if p.PromptPattern() != nil {
		t.Fatal("PromptPattern() should be nil for server provider")
	}
	if p.StartupTimeout() != 30*time.Second {
		t.Fatalf("StartupTimeout() = %v, want 30s", p.StartupTimeout())
	}
	if p.StopGrace() != 10*time.Second {
		t.Fatalf("StopGrace() = %v, want 10s", p.StopGrace())
	}
	if !p.IsStreamJSON() {
		t.Fatal("IsStreamJSON() should be true")
	}
	if p.cfg.Hostname != "127.0.0.1" {
		t.Fatalf("hostname = %q, want %q", p.cfg.Hostname, "127.0.0.1")
	}
	if p.cfg.PortRangeStart != 4100 {
		t.Fatalf("PortRangeStart = %d, want 4100", p.cfg.PortRangeStart)
	}
	if p.cfg.PortRangeEnd != 4200 {
		t.Fatalf("PortRangeEnd = %d, want 4200", p.cfg.PortRangeEnd)
	}
}

func TestNewOpenCodeServerProvider_CustomConfig(t *testing.T) {
	p := NewOpenCodeServerProvider(OpenCodeServerConfig{
		ProviderID:     "opencode-server",
		Binary:         "/usr/bin/opencode",
		StartupTimeout: 60 * time.Second,
		StopGrace:      20 * time.Second,
		Hostname:       "0.0.0.0",
		PortRangeStart: 5000,
		PortRangeEnd:   5100,
	})

	if p.StartupTimeout() != 60*time.Second {
		t.Fatalf("StartupTimeout() = %v, want 60s", p.StartupTimeout())
	}
	if p.StopGrace() != 20*time.Second {
		t.Fatalf("StopGrace() = %v, want 20s", p.StopGrace())
	}
	if p.cfg.Hostname != "0.0.0.0" {
		t.Fatalf("hostname = %q, want %q", p.cfg.Hostname, "0.0.0.0")
	}
	if p.cfg.PortRangeStart != 5000 {
		t.Fatalf("PortRangeStart = %d, want 5000", p.cfg.PortRangeStart)
	}
}

func TestOpenCodeServerProvider_ValidateStartup(t *testing.T) {
	t.Run("missing required env", func(t *testing.T) {
		t.Setenv("OPENAI_API_KEY", "")
		_ = os.Unsetenv("OPENAI_API_KEY")
		p := NewOpenCodeServerProvider(OpenCodeServerConfig{
			ProviderID:  "opencode-server",
			Binary:      "/bin/echo",
			RequiredEnv: []string{"OPENAI_API_KEY"},
		})

		err := p.ValidateStartup(context.Background())
		if err == nil {
			t.Fatal("expected error when required env is missing")
		}
		if !strings.Contains(err.Error(), "OPENAI_API_KEY") {
			t.Fatalf("error should mention OPENAI_API_KEY, got: %v", err)
		}
	})

	t.Run("env present", func(t *testing.T) {
		t.Setenv("OPENAI_API_KEY", "sk-test")
		p := NewOpenCodeServerProvider(OpenCodeServerConfig{
			ProviderID:  "opencode-server",
			Binary:      "/bin/echo",
			RequiredEnv: []string{"OPENAI_API_KEY"},
		})

		if err := p.ValidateStartup(context.Background()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestOpenCodeServerProvider_Health(t *testing.T) {
	p := NewOpenCodeServerProvider(OpenCodeServerConfig{
		ProviderID: "opencode-server",
		Binary:     "/bin/echo",
	})

	if err := p.Health(context.Background()); err != nil {
		t.Fatalf("Health() failed: %v", err)
	}
}

func TestOpenCodeServerProvider_Health_Unavailable(t *testing.T) {
	p := NewOpenCodeServerProvider(OpenCodeServerConfig{
		ProviderID: "opencode-server",
		Binary:     "/bin/echo",
	})
	p.SetUnavailable(fmt.Errorf("test unavailable"))

	err := p.Health(context.Background())
	if err == nil {
		t.Fatal("expected error when provider is unavailable")
	}
	if !strings.Contains(err.Error(), "test unavailable") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestOpenCodeServerProvider_Health_BinaryNotFound(t *testing.T) {
	p := NewOpenCodeServerProvider(OpenCodeServerConfig{
		ProviderID: "opencode-server",
		Binary:     "/nonexistent/binary",
	})

	err := p.Health(context.Background())
	if err == nil {
		t.Fatal("expected error for missing binary")
	}
}

func TestOpenCodeServerProvider_Health_RequiredEnvMissing(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	_ = os.Unsetenv("OPENAI_API_KEY")
	p := NewOpenCodeServerProvider(OpenCodeServerConfig{
		ProviderID:  "opencode-server",
		Binary:      "/bin/echo",
		RequiredEnv: []string{"OPENAI_API_KEY"},
	})

	err := p.HealthWithEnv(context.Background(), []string{"PATH=/usr/bin"})
	if err == nil {
		t.Fatal("expected error when required env is missing")
	}
	if !strings.Contains(err.Error(), "OPENAI_API_KEY") {
		t.Fatalf("error should mention OPENAI_API_KEY, got: %v", err)
	}
}

func TestOpenCodeServerProvider_BuildCommand(t *testing.T) {
	p := NewOpenCodeServerProvider(OpenCodeServerConfig{
		ProviderID:     "opencode-server",
		Binary:         "/bin/echo",
		PortRangeStart: 14100,
		PortRangeEnd:   14200,
	})

	cfg := bridge.SessionConfig{
		ProjectID: "test-project",
		SessionID: "sess-1",
		RepoPath:  t.TempDir(),
	}
	cmd, err := p.BuildCommand(context.Background(), cfg)
	if err != nil {
		t.Fatalf("BuildCommand: %v", err)
	}

	// Verify command includes "serve" subcommand, --hostname, and --port.
	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "serve") {
		t.Fatalf("expected 'serve' in args, got: %s", args)
	}
	if !strings.Contains(args, "--hostname") {
		t.Fatalf("expected '--hostname' in args, got: %s", args)
	}
	if !strings.Contains(args, "--port") {
		t.Fatalf("expected '--port' in args, got: %s", args)
	}

	// Verify session metadata was stored.
	baseURL, err := p.SessionBaseURL("sess-1")
	if err != nil {
		t.Fatalf("SessionBaseURL: %v", err)
	}
	if !strings.HasPrefix(baseURL, "http://127.0.0.1:") {
		t.Fatalf("unexpected base URL: %s", baseURL)
	}

	// Verify command Dir is set to repo path.
	if cmd.Dir != cfg.RepoPath {
		t.Fatalf("cmd.Dir = %q, want %q", cmd.Dir, cfg.RepoPath)
	}
}

func TestOpenCodeServerProvider_BuildCommand_WithSessionEnv(t *testing.T) {
	p := NewOpenCodeServerProvider(OpenCodeServerConfig{
		ProviderID:     "opencode-server",
		Binary:         "/bin/echo",
		PortRangeStart: 14200,
		PortRangeEnd:   14300,
	})

	customEnv := []string{"OPENAI_API_KEY=sk-test", "PATH=/usr/bin"}
	cfg := bridge.SessionConfig{
		ProjectID: "test-project",
		SessionID: "sess-env",
		RepoPath:  t.TempDir(),
		Env:       customEnv,
	}
	cmd, err := p.BuildCommand(context.Background(), cfg)
	if err != nil {
		t.Fatalf("BuildCommand: %v", err)
	}

	// Verify the env was passed through.
	found := false
	for _, e := range cmd.Env {
		if e == "OPENAI_API_KEY=sk-test" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected OPENAI_API_KEY=sk-test in cmd.Env")
	}
}

func TestOpenCodeServerProvider_BuildCommand_BinaryNotOnPath(t *testing.T) {
	// When a binary name without slashes is given and it cannot be found on
	// PATH, resolveBinaryPath returns an error.
	p := NewOpenCodeServerProvider(OpenCodeServerConfig{
		ProviderID: "opencode-server",
		Binary:     "nonexistent-binary-that-does-not-exist",
	})

	cfg := bridge.SessionConfig{
		ProjectID: "test-project",
		SessionID: "sess-fail",
		RepoPath:  t.TempDir(),
	}
	_, err := p.BuildCommand(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error for binary not found on PATH")
	}
}

func TestOpenCodeServerProvider_AllocatePort(t *testing.T) {
	p := NewOpenCodeServerProvider(OpenCodeServerConfig{
		ProviderID:     "opencode-server",
		Binary:         "/bin/echo",
		Hostname:       "127.0.0.1",
		PortRangeStart: 14300,
		PortRangeEnd:   14310,
	})

	port, err := p.allocatePort()
	if err != nil {
		t.Fatalf("allocatePort: %v", err)
	}
	if port < 14300 || port >= 14310 {
		t.Fatalf("port %d out of expected range [14300, 14310)", port)
	}
}

func TestOpenCodeServerProvider_AllocatePort_RangeFull(t *testing.T) {
	// Block all ports in the range, then verify OS fallback works.
	listeners := make([]net.Listener, 0)
	defer func() {
		for _, ln := range listeners {
			_ = ln.Close()
		}
	}()

	for port := 14400; port < 14405; port++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			t.Skipf("could not bind port %d: %v", port, err)
		}
		listeners = append(listeners, ln)
	}

	p := NewOpenCodeServerProvider(OpenCodeServerConfig{
		ProviderID:     "opencode-server",
		Binary:         "/bin/echo",
		Hostname:       "127.0.0.1",
		PortRangeStart: 14400,
		PortRangeEnd:   14405,
	})

	port, err := p.allocatePort()
	if err != nil {
		t.Fatalf("allocatePort with full range: %v", err)
	}
	// OS-assigned port should be outside the configured range.
	if port >= 14400 && port < 14405 {
		t.Fatalf("expected OS-assigned port outside range, got %d", port)
	}
	if port <= 0 {
		t.Fatalf("invalid port: %d", port)
	}
}

func TestOpenCodeServerProvider_AllocatePort_SkipsTrackedSessions(t *testing.T) {
	p := NewOpenCodeServerProvider(OpenCodeServerConfig{
		ProviderID:     "opencode-server",
		Binary:         "/bin/echo",
		Hostname:       "127.0.0.1",
		PortRangeStart: 14600,
		PortRangeEnd:   14605,
	})

	// Pre-track a session on port 14600.
	p.mu.Lock()
	p.sessions["existing-sess"] = &openCodeSessionMeta{
		Port:    14600,
		BaseURL: "http://127.0.0.1:14600",
	}
	p.mu.Unlock()

	port, err := p.allocatePort()
	if err != nil {
		t.Fatalf("allocatePort: %v", err)
	}
	// The allocated port must not be the one already in use by the tracked session.
	if port == 14600 {
		t.Fatalf("allocatePort returned tracked port %d", port)
	}
	if port < 14601 || port >= 14605 {
		// It should pick from the remaining range ports.
		t.Fatalf("expected port in [14601, 14605), got %d", port)
	}
}

func TestOpenCodeServerProvider_SubscribeSSE_UnknownSession(t *testing.T) {
	p := NewOpenCodeServerProvider(OpenCodeServerConfig{
		ProviderID: "opencode-server",
		Binary:     "/bin/echo",
	})

	_, err := p.SubscribeSSE(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown session")
	}
}

func TestOpenCodeServerProvider_SubscribeSSE_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := NewOpenCodeServerProvider(OpenCodeServerConfig{
		ProviderID: "opencode-server",
		Binary:     "/bin/echo",
	})

	p.mu.Lock()
	p.sessions["sess-sse-err"] = &openCodeSessionMeta{
		Port:    0,
		BaseURL: srv.URL,
	}
	p.mu.Unlock()

	_, err := p.SubscribeSSE(context.Background(), "sess-sse-err")
	if err == nil {
		t.Fatal("expected error for non-200 SSE response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("expected status code in error, got: %v", err)
	}
}

func TestParseSSEStream_ContextCancelled(t *testing.T) {
	input := "event:msg\ndata:payload\n\nevent:msg2\ndata:payload2\n\n"
	reader := strings.NewReader(input)
	ch := make(chan SSEEvent, 10)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	parseSSEStream(ctx, reader, ch)
	close(ch)

	var events []SSEEvent
	for evt := range ch {
		events = append(events, evt)
	}
	// With context cancelled, we may get 0 or some events; the key thing is it
	// does not hang.
	if len(events) > 2 {
		t.Fatalf("expected at most 2 events with cancelled context, got %d", len(events))
	}
}

func TestParseSSEStream_EmptyDataLines(t *testing.T) {
	// Lines that don't match any prefix and empty events should be skipped.
	input := "unknown:line\n\nevent:msg\ndata:payload\n\n\n\n"
	reader := strings.NewReader(input)
	ch := make(chan SSEEvent, 10)

	parseSSEStream(context.Background(), reader, ch)
	close(ch)

	var events []SSEEvent
	for evt := range ch {
		events = append(events, evt)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Event != "msg" || events[0].Data != "payload" {
		t.Fatalf("unexpected event: %+v", events[0])
	}
}

func TestOpenCodeServerProvider_SessionBaseURL_Unknown(t *testing.T) {
	p := NewOpenCodeServerProvider(OpenCodeServerConfig{
		ProviderID: "opencode-server",
		Binary:     "/bin/echo",
	})

	_, err := p.SessionBaseURL("nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown session")
	}
}

func TestOpenCodeServerProvider_CleanupSession(t *testing.T) {
	p := NewOpenCodeServerProvider(OpenCodeServerConfig{
		ProviderID:     "opencode-server",
		Binary:         "/bin/echo",
		PortRangeStart: 14500,
		PortRangeEnd:   14510,
	})

	cfg := bridge.SessionConfig{
		ProjectID: "test",
		SessionID: "sess-cleanup",
		RepoPath:  t.TempDir(),
	}
	_, err := p.BuildCommand(context.Background(), cfg)
	if err != nil {
		t.Fatalf("BuildCommand: %v", err)
	}

	// Session should be tracked.
	if _, err := p.SessionBaseURL("sess-cleanup"); err != nil {
		t.Fatalf("session should be tracked: %v", err)
	}

	p.CleanupSession("sess-cleanup")

	// Session should no longer be tracked.
	if _, err := p.SessionBaseURL("sess-cleanup"); err == nil {
		t.Fatal("expected error after cleanup")
	}
}

func TestOpenCodeServerProvider_WaitForHealth(t *testing.T) {
	// Start a fake health server.
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if r.URL.Path == "/global/health" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	p := NewOpenCodeServerProvider(OpenCodeServerConfig{
		ProviderID: "opencode-server",
		Binary:     "/bin/echo",
	})

	// Manually inject session metadata pointing at the test server.
	p.mu.Lock()
	p.sessions["sess-health"] = &openCodeSessionMeta{
		Port:    0,
		BaseURL: srv.URL,
	}
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := p.WaitForHealth(ctx, "sess-health"); err != nil {
		t.Fatalf("WaitForHealth: %v", err)
	}
	if callCount == 0 {
		t.Fatal("expected at least one health check call")
	}
}

func TestOpenCodeServerProvider_WaitForHealth_Timeout(t *testing.T) {
	// Start a server that never returns 200.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	p := NewOpenCodeServerProvider(OpenCodeServerConfig{
		ProviderID: "opencode-server",
		Binary:     "/bin/echo",
	})

	p.mu.Lock()
	p.sessions["sess-timeout"] = &openCodeSessionMeta{
		Port:    0,
		BaseURL: srv.URL,
	}
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := p.WaitForHealth(ctx, "sess-timeout")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout error, got: %v", err)
	}
}

func TestOpenCodeServerProvider_WaitForHealth_UnknownSession(t *testing.T) {
	p := NewOpenCodeServerProvider(OpenCodeServerConfig{
		ProviderID: "opencode-server",
		Binary:     "/bin/echo",
	})

	err := p.WaitForHealth(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown session")
	}
}

func TestOpenCodeServerProvider_SendPrompt(t *testing.T) {
	var receivedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session/prompt" && r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			receivedBody = string(body)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	p := NewOpenCodeServerProvider(OpenCodeServerConfig{
		ProviderID: "opencode-server",
		Binary:     "/bin/echo",
	})

	p.mu.Lock()
	p.sessions["sess-prompt"] = &openCodeSessionMeta{
		Port:    0,
		BaseURL: srv.URL,
	}
	p.mu.Unlock()

	err := p.SendPrompt(context.Background(), "sess-prompt", "hello world")
	if err != nil {
		t.Fatalf("SendPrompt: %v", err)
	}
	if !strings.Contains(receivedBody, "hello world") {
		t.Fatalf("expected prompt in body, got: %s", receivedBody)
	}
}

func TestOpenCodeServerProvider_SendPrompt_UnknownSession(t *testing.T) {
	p := NewOpenCodeServerProvider(OpenCodeServerConfig{
		ProviderID: "opencode-server",
		Binary:     "/bin/echo",
	})

	err := p.SendPrompt(context.Background(), "nonexistent", "test")
	if err == nil {
		t.Fatal("expected error for unknown session")
	}
	if !strings.Contains(err.Error(), "no session metadata") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestOpenCodeServerProvider_SendPrompt_SpecialChars(t *testing.T) {
	// Verify JSON-injection-safe prompt encoding: prompts with quotes,
	// backslashes, and newlines must not corrupt the JSON payload.
	var receivedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session/prompt" && r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			receivedBody = string(body)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	p := NewOpenCodeServerProvider(OpenCodeServerConfig{
		ProviderID: "opencode-server",
		Binary:     "/bin/echo",
	})

	p.mu.Lock()
	p.sessions["sess-special"] = &openCodeSessionMeta{
		Port:    0,
		BaseURL: srv.URL,
	}
	p.mu.Unlock()

	// This prompt contains characters that would break naive fmt.Sprintf JSON.
	prompt := `say "hello\nworld" and use a backslash \`
	err := p.SendPrompt(context.Background(), "sess-special", prompt)
	if err != nil {
		t.Fatalf("SendPrompt: %v", err)
	}

	// Verify the body is valid JSON by unmarshalling it.
	var parsed struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(receivedBody), &parsed); err != nil {
		t.Fatalf("received body is not valid JSON: %v\nbody: %s", err, receivedBody)
	}
	if parsed.Content != prompt {
		t.Fatalf("round-tripped content = %q, want %q", parsed.Content, prompt)
	}
}

func TestOpenCodeServerProvider_SendPrompt_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal"}`))
	}))
	defer srv.Close()

	p := NewOpenCodeServerProvider(OpenCodeServerConfig{
		ProviderID: "opencode-server",
		Binary:     "/bin/echo",
	})

	p.mu.Lock()
	p.sessions["sess-err"] = &openCodeSessionMeta{
		Port:    0,
		BaseURL: srv.URL,
	}
	p.mu.Unlock()

	err := p.SendPrompt(context.Background(), "sess-err", "test")
	if err == nil {
		t.Fatal("expected error for server error response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("expected status 500 in error, got: %v", err)
	}
}

func TestOpenCodeServerProvider_AbortTurn_UnknownSession(t *testing.T) {
	p := NewOpenCodeServerProvider(OpenCodeServerConfig{
		ProviderID: "opencode-server",
		Binary:     "/bin/echo",
	})

	err := p.AbortTurn(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown session")
	}
}

func TestOpenCodeServerProvider_AbortTurn_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"bad gateway"}`))
	}))
	defer srv.Close()

	p := NewOpenCodeServerProvider(OpenCodeServerConfig{
		ProviderID: "opencode-server",
		Binary:     "/bin/echo",
	})

	p.mu.Lock()
	p.sessions["sess-abort-err"] = &openCodeSessionMeta{
		Port:    0,
		BaseURL: srv.URL,
	}
	p.mu.Unlock()

	err := p.AbortTurn(context.Background(), "sess-abort-err")
	if err == nil {
		t.Fatal("expected error for server error response")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Fatalf("expected status 502 in error, got: %v", err)
	}
}

func TestOpenCodeServerProvider_AbortTurn(t *testing.T) {
	aborted := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session/abort" && r.Method == http.MethodPost {
			aborted = true
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	p := NewOpenCodeServerProvider(OpenCodeServerConfig{
		ProviderID: "opencode-server",
		Binary:     "/bin/echo",
	})

	p.mu.Lock()
	p.sessions["sess-abort"] = &openCodeSessionMeta{
		Port:    0,
		BaseURL: srv.URL,
	}
	p.mu.Unlock()

	err := p.AbortTurn(context.Background(), "sess-abort")
	if err != nil {
		t.Fatalf("AbortTurn: %v", err)
	}
	if !aborted {
		t.Fatal("abort endpoint was not called")
	}
}

func TestOpenCodeServerProvider_SubscribeSSE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/event" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, ok := w.(http.Flusher)
			if !ok {
				return
			}
			_, _ = fmt.Fprint(w, "event:message\ndata:{\"text\":\"hello\"}\n\n")
			flusher.Flush()
			_, _ = fmt.Fprint(w, "event:done\ndata:{}\n\n")
			flusher.Flush()
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	p := NewOpenCodeServerProvider(OpenCodeServerConfig{
		ProviderID: "opencode-server",
		Binary:     "/bin/echo",
	})

	p.mu.Lock()
	p.sessions["sess-sse"] = &openCodeSessionMeta{
		Port:    0,
		BaseURL: srv.URL,
	}
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ch, err := p.SubscribeSSE(ctx, "sess-sse")
	if err != nil {
		t.Fatalf("SubscribeSSE: %v", err)
	}

	var events []SSEEvent
	for evt := range ch {
		events = append(events, evt)
	}

	if len(events) < 1 {
		t.Fatalf("expected at least 1 event, got %d", len(events))
	}
	if events[0].Event != "message" {
		t.Fatalf("first event type = %q, want %q", events[0].Event, "message")
	}
	if !strings.Contains(events[0].Data, "hello") {
		t.Fatalf("first event data should contain 'hello', got: %s", events[0].Data)
	}
}

func TestParseSSEStream(t *testing.T) {
	input := "event:msg\ndata:{\"text\":\"hi\"}\nid:1\n\nevent:end\ndata:{}\n\n"
	reader := strings.NewReader(input)
	ch := make(chan SSEEvent, 10)

	parseSSEStream(context.Background(), reader, ch)
	close(ch)

	var events []SSEEvent
	for evt := range ch {
		events = append(events, evt)
	}

	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].Event != "msg" {
		t.Fatalf("events[0].Event = %q, want %q", events[0].Event, "msg")
	}
	if events[0].Data != `{"text":"hi"}` {
		t.Fatalf("events[0].Data = %q", events[0].Data)
	}
	if events[0].ID != "1" {
		t.Fatalf("events[0].ID = %q, want %q", events[0].ID, "1")
	}
	if events[1].Event != "end" {
		t.Fatalf("events[1].Event = %q, want %q", events[1].Event, "end")
	}
}

func TestOpenCodeServerCapabilities(t *testing.T) {
	caps := OpenCodeServerCapabilities()
	if len(caps) < 2 {
		t.Fatalf("expected at least 2 capabilities, got %d", len(caps))
	}

	found := make(map[string]bool)
	for _, c := range caps {
		found[c] = true
	}
	for _, want := range []string{"message_input", "structured_events"} {
		if !found[want] {
			t.Fatalf("missing capability %q", want)
		}
	}
}

func TestOpenCodeServerProvider_Version(t *testing.T) {
	p := NewOpenCodeServerProvider(OpenCodeServerConfig{
		ProviderID: "opencode-server",
		Binary:     "/bin/echo",
	})

	v, err := p.Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	// /bin/echo --version outputs something; just check it's non-empty.
	if v == "" {
		t.Fatal("expected non-empty version")
	}
}

func TestOpenCodeServerProvider_HealthWithEnv(t *testing.T) {
	p := NewOpenCodeServerProvider(OpenCodeServerConfig{
		ProviderID:  "opencode-server",
		Binary:      "/bin/echo",
		RequiredEnv: []string{"TEST_KEY"},
	})

	env := []string{"PATH=/usr/bin", "TEST_KEY=value"}
	if err := p.HealthWithEnv(context.Background(), env); err != nil {
		t.Fatalf("HealthWithEnv: %v", err)
	}
}

func TestOpenCodeServerProvider_BuildCommand_IPv6(t *testing.T) {
	// When the hostname is an IPv6 address the base URL must wrap it in
	// brackets so that the port is not confused with the address, e.g.
	// http://[::1]:4100 rather than http://::1:4100.
	p := NewOpenCodeServerProvider(OpenCodeServerConfig{
		ProviderID:     "opencode-server",
		Binary:         "/bin/echo",
		Hostname:       "::1",
		PortRangeStart: 14700,
		PortRangeEnd:   14800,
	})

	cfg := bridge.SessionConfig{
		ProjectID: "test-project",
		SessionID: "sess-ipv6",
		RepoPath:  t.TempDir(),
	}
	_, err := p.BuildCommand(context.Background(), cfg)
	if err != nil {
		t.Fatalf("BuildCommand: %v", err)
	}

	baseURL, err := p.SessionBaseURL("sess-ipv6")
	if err != nil {
		t.Fatalf("SessionBaseURL: %v", err)
	}
	// The URL must contain brackets around the IPv6 address.
	if !strings.HasPrefix(baseURL, "http://[::1]:") {
		t.Fatalf("expected IPv6-bracketed base URL, got: %s", baseURL)
	}
}

func TestParseSSEStream_LargeDataLine(t *testing.T) {
	// Verify that SSE data lines larger than the default 64 KB scanner
	// buffer are not silently truncated.
	bigPayload := strings.Repeat("x", 128*1024) // 128 KB
	input := fmt.Sprintf("event:big\ndata:%s\n\n", bigPayload)
	reader := strings.NewReader(input)
	ch := make(chan SSEEvent, 10)

	parseSSEStream(context.Background(), reader, ch)
	close(ch)

	var events []SSEEvent
	for evt := range ch {
		events = append(events, evt)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Data != bigPayload {
		t.Fatalf("data length = %d, want %d (truncated)", len(events[0].Data), len(bigPayload))
	}
}
