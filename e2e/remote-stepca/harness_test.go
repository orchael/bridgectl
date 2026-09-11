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
    client_failure|already_exited) echo 1 ;;
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
		})
	}
}
