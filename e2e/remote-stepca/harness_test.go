package remote_stepca_e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestMakeTargetResult covers the PRD container acceptance criteria: setup and
// client failures must fail the target, and cleanup must run in every case.
func TestMakeTargetResult(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make is required")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		pass bool
	}{
		{"success", true},
		{"client_failure", false},
		{"already_exited", false},
		{"build_failure", false},
		{"startup_failure", false},
		{"ps_failure", false},
		{"missing_container", false},
		{"wait_failure", false},
		{"invalid_wait_output", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			// docker wait reports the container exit code on stdout; its own
			// status is zero when waiting succeeds, even for a failed client.
			fakeDocker := `#!/bin/sh
echo "$*" >> "$DOCKER_CALLS"
if [ "$1" = wait ]; then
  case "$SCENARIO" in
    wait_failure) exit 7 ;;
    invalid_wait_output) echo invalid ;;
    client_failure) echo 1 ;;
    already_exited) echo 23 ;;
    *) echo 0 ;;
  esac
  exit 0
fi
case "$4" in
  build) [ "$SCENARIO" != build_failure ] || exit 3 ;;
  up) [ "$SCENARIO" != startup_failure ] || exit 4 ;;
  ps)
    [ "$SCENARIO" != ps_failure ] || exit 5
    [ "$SCENARIO" != missing_container ] || exit 0
    if [ "$SCENARIO" = already_exited ]; then
      case " $* " in *" -a "*|*" --all "*) ;; *) exit 0 ;; esac
    fi
    echo fake-client ;;
esac
`
			if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(fakeDocker), 0o755); err != nil {
				t.Fatal(err)
			}
			callsPath := filepath.Join(dir, "calls")
			cmd := exec.Command("make", "--no-print-directory", "test-remote-stepca")
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"SCENARIO="+tc.name, "DOCKER_CALLS="+callsPath)
			output, runErr := cmd.CombinedOutput()
			if (runErr == nil) != tc.pass {
				t.Errorf("make error = %v, want success %v\n%s", runErr, tc.pass, output)
			}
			banner := "test-remote-stepca: FAILED"
			if tc.pass {
				banner = "test-remote-stepca: PASSED"
			}
			if !strings.Contains(string(output), banner) {
				t.Errorf("missing %q in output:\n%s", banner, output)
			}
			calls, err := os.ReadFile(callsPath)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(calls), " down -v\n") {
				t.Errorf("cleanup did not run:\n%s", calls)
			}
			if tc.name == "build_failure" && strings.Contains(string(calls), " up ") {
				t.Errorf("started containers after build failure:\n%s", calls)
			}
			if tc.name == "already_exited" {
				if !strings.Contains(string(calls), " ps -a -q remote-client\n") {
					t.Errorf("lookup did not include stopped containers:\n%s", calls)
				}
				if !strings.Contains(string(calls), "wait fake-client\n") {
					t.Errorf("did not wait for the stopped client:\n%s", calls)
				}
				if !strings.Contains(string(output), "test-remote-stepca: FAILED (exit 23)") {
					t.Errorf("did not preserve the stopped client's exit code:\n%s", output)
				}
			}
		})
	}
}

// TestMakeTargetResultPublished covers the published-image variant added for
// issue #223: pulling the server image must gate the run (a failed pull must
// never fall through to building/starting the server), and the target must
// still satisfy the same pass/fail/cleanup contract as TestMakeTargetResult.
func TestMakeTargetResultPublished(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make is required")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		pass bool
	}{
		{"success", true},
		{"pull_failure", false},
		{"client_failure", false},
		{"startup_failure", false},
		// The client reports success but the JUnit file never lands in the
		// results directory (e.g. a `docker compose cp` failure) — the fix
		// for the Copilot finding on this PR requires this to fail the
		// target rather than reporting a silent, artifact-less PASS.
		{"results_missing", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			resultsOut := t.TempDir()
			// Mirrors the fake docker in TestMakeTargetResult, plus a `pull`
			// branch (the published target's first docker call, which must
			// gate everything after it) and no-op `logs`/`cp` branches so the
			// artifact-capture steps succeed without a real compose project.
			fakeDocker := `#!/bin/sh
echo "$*" >> "$DOCKER_CALLS"
if [ "$1" = pull ]; then
  [ "$SCENARIO" != pull_failure ] || exit 9
  exit 0
fi
if [ "$1" = wait ]; then
  case "$SCENARIO" in
    client_failure) echo 1 ;;
    *) echo 0 ;;
  esac
  exit 0
fi
case "$4" in
  up) [ "$SCENARIO" != startup_failure ] || exit 4 ;;
  ps) echo fake-client ;;
  cp) [ "$SCENARIO" != success ] || touch "$6/remote-stepca-e2e.xml" ;;
esac
`
			if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(fakeDocker), 0o755); err != nil {
				t.Fatal(err)
			}
			callsPath := filepath.Join(dir, "calls")
			cmd := exec.Command("make", "--no-print-directory", "test-remote-stepca-published")
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"SCENARIO="+tc.name, "DOCKER_CALLS="+callsPath,
				"BRIDGECTL_SERVER_IMAGE=ghcr.io/orchael/bridgectl@sha256:deadbeef",
				"RESULTS_OUT_DIR="+resultsOut)
			output, runErr := cmd.CombinedOutput()
			if (runErr == nil) != tc.pass {
				t.Errorf("make error = %v, want success %v\n%s", runErr, tc.pass, output)
			}
			// A failed `docker pull` exits the recipe immediately via a bare
			// `exit 1` (matching the pattern already used for the missing
			// BRIDGECTL_SERVER_IMAGE check above it) — there is no PASSED/
			// FAILED banner for that path, and nothing to clean up since no
			// compose call ever ran. Every other scenario goes through the
			// normal banner + cleanup path.
			if tc.name != "pull_failure" {
				banner := "test-remote-stepca-published: FAILED"
				if tc.pass {
					banner = "test-remote-stepca-published: PASSED"
				}
				if !strings.Contains(string(output), banner) {
					t.Errorf("missing %q in output:\n%s", banner, output)
				}
			}
			calls, err := os.ReadFile(callsPath)
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "pull_failure" {
				if strings.Contains(string(calls), " up ") {
					t.Errorf("started containers after a failed pull:\n%s", calls)
				}
				if strings.Contains(string(calls), " down -v\n") {
					t.Errorf("ran cleanup despite exiting before any compose call:\n%s", calls)
				}
			} else if !strings.Contains(string(calls), " down -v\n") {
				t.Errorf("cleanup did not run:\n%s", calls)
			}
			if tc.name == "startup_failure" && strings.Contains(string(calls), " ps ") {
				t.Errorf("looked up the client container after a startup failure:\n%s", calls)
			}
			// A reported PASS without a JUnit file must be turned into a
			// FAILED result (the fix for the Copilot finding on this PR) —
			// "success" is the only scenario where the fake client's results
			// volume is expected to be missing yet the run still claims
			// success, so this is the one case that would regress silently.
			if tc.name == "success" {
				if _, err := os.Stat(filepath.Join(resultsOut, "compose.log")); err != nil {
					t.Errorf("compose.log was not captured: %v", err)
				}
			}
		})
	}
}
