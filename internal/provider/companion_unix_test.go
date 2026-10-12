//go:build !windows

package provider

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// A launcher with a child holding a socket models codex.js and its native
// app-server. Killing only the launcher leaves the socket (and locks) held.
func TestCompanionProcessCleanup(t *testing.T) {
	for _, cancelContext := range []bool{false, true} {
		name := "startup_failure"
		if cancelContext {
			name = "session_cancel"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			path := filepath.Join(t.TempDir(), "socket")
			cmd := exec.CommandContext(ctx, "sh", "-c", `"$1" -test.run=^TestCompanionSocketHelper$ & wait`, "launcher", os.Args[0])
			cmd.Env = append(os.Environ(), "BRIDGECTL_TEST_COMPANION_SOCKET="+path)
			configureCompanionProcess(cmd)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = killCompanionProcess(cmd)
				_ = cmd.Wait()
			})
			deadline := time.Now().Add(5 * time.Second)
			var conn net.Conn
			for time.Now().Before(deadline) {
				var err error
				conn, err = net.DialTimeout("unix", path, 100*time.Millisecond)
				if err == nil {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if conn == nil {
				t.Fatal("companion child did not open its socket")
			}
			defer func() { _ = conn.Close() }()
			if cancelContext {
				cancel()
			} else if err := killCompanionProcess(cmd); err != nil {
				t.Fatal(err)
			}
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, err := conn.Read(make([]byte, 1))
			if err == nil {
				t.Fatal("unexpected data from companion")
			}
			if e, ok := err.(net.Error); ok && e.Timeout() {
				t.Fatal("companion child survived cleanup and still holds socket")
			}
		})
	}
}

func TestCompanionSocketHelper(t *testing.T) {
	path := os.Getenv("BRIDGECTL_TEST_COMPANION_SOCKET")
	if path == "" {
		return
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	conn, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = conn.Read(make([]byte, 1))
}
