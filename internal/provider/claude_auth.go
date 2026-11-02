package provider

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Claude accepts either an explicit CLAUDE_CODE_OAUTH_TOKEN or the native
// credentials the `claude` CLI writes for itself, mirroring how
// resolveCodexAuth treats a desktop-local auth.json as valid authentication.
// Requiring the token unconditionally reports an interactive developer who
// has already run `claude setup-token` or logged in as having no credentials
// at all, even though the CLI bridgectl launches would authenticate fine:
// FilterEnv is a blocklist, so HOME reaches the child untouched and the CLI
// reads its own store.
//
// Unlike Codex, bridgectl never seeds or rewrites Claude credentials. The CLI
// owns its store (a file on Linux, the login keychain on macOS); this only
// decides whether the provider is available, so no secret is read here.
const (
	claudeCredentialsFile = ".credentials.json"
	claudeKeychainService = "Claude Code-credentials"
)

// claudeAuthKind labels which credential source satisfied the provider, for
// error messages and tests. It is never used to choose launch behaviour.
type claudeAuthKind string

const (
	claudeAuthToken       claudeAuthKind = "token"
	claudeAuthCredentials claudeAuthKind = "credentials-file"
	claudeAuthKeychain    claudeAuthKind = "keychain"
)

type claudeAuthSource struct {
	kind claudeAuthKind
	// path is the credentials file that satisfied the check, empty for the
	// token and keychain sources.
	path string
}

// claudeUserHome mirrors codexUserHome: resolve HOME from the environment the
// child will actually receive, falling back to USERPROFILE on Windows.
func claudeUserHome(env []string, platform string) string {
	home := strings.TrimSpace(envValue(env, "HOME"))
	if home == "" && platform == "windows" {
		home = strings.TrimSpace(envValue(env, "USERPROFILE"))
	}
	return home
}

// claudeCredentialsValid reports whether data carries a non-empty OAuth access
// token. Like codexAuthKind it checks the stored shape only, never server
// validity, so a logged-out placeholder or a truncated write does not count as
// authentication. Both the nested claudeAiOauth envelope the CLI writes and a
// bare accessToken are accepted so a future layout change degrades to
// "unavailable with a clear reason" rather than a false positive.
func claudeCredentialsValid(data []byte) bool {
	var creds struct {
		ClaudeAIOAuth *struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
		AccessToken string `json:"accessToken"`
	}
	if json.Unmarshal(data, &creds) != nil {
		return false
	}
	if creds.ClaudeAIOAuth != nil && strings.TrimSpace(creds.ClaudeAIOAuth.AccessToken) != "" {
		return true
	}
	return strings.TrimSpace(creds.AccessToken) != ""
}

// claudeKeychainLookup reports whether the login keychain holds a Claude Code
// credential. It queries attributes only — never -g, which would read the
// secret and can raise an interactive keychain prompt on a machine running
// bridgectl as a background service.
func claudeKeychainLookup(service string) bool {
	path, err := exec.LookPath("security")
	if err != nil {
		return false
	}
	return exec.Command(path, "find-generic-password", "-s", service).Run() == nil
}

func resolveClaudeAuth(env []string) (claudeAuthSource, error) {
	return resolveClaudeAuthWith(env, runtime.GOOS, claudeKeychainLookup)
}

// resolveClaudeAuthWith takes the platform and keychain probe as parameters so
// tests can cover every branch on any host.
func resolveClaudeAuthWith(env []string, platform string, keychainLookup func(string) bool) (claudeAuthSource, error) {
	if strings.TrimSpace(envValue(env, "CLAUDE_CODE_OAUTH_TOKEN")) != "" {
		return claudeAuthSource{kind: claudeAuthToken}, nil
	}

	if home := claudeUserHome(env, platform); home != "" {
		credentials := filepath.Join(home, ".claude", claudeCredentialsFile)
		data, err := os.ReadFile(credentials)
		if err != nil && !os.IsNotExist(err) {
			return claudeAuthSource{}, fmt.Errorf("read claude credentials file: %w", err)
		}
		if claudeCredentialsValid(data) {
			return claudeAuthSource{kind: claudeAuthCredentials, path: credentials}, nil
		}
	}

	// macOS stores the credential in the login keychain instead of the file
	// above, so a file-only check would report an interactive developer who is
	// signed in as unauthenticated.
	if platform == "darwin" && keychainLookup != nil && keychainLookup(claudeKeychainService) {
		return claudeAuthSource{kind: claudeAuthKeychain}, nil
	}

	return claudeAuthSource{}, fmt.Errorf(
		"claude requires CLAUDE_CODE_OAUTH_TOKEN or native CLI credentials; run 'claude setup-token' or 'claude' to log in")
}
