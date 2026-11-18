package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

const testEnrollKey = "bek_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestMachineCredentialFromEnv(t *testing.T) {
	client := map[string]string{envClientID: "id", envClientPass: "s3cret", envIssuer: "https://auth.example.test", envProjectID: "proj-1"}
	with := func(base map[string]string, k, v string) map[string]string {
		m := map[string]string{}
		for a, b := range base {
			m[a] = b
		}
		if v == "" {
			delete(m, k)
		} else {
			m[k] = v
		}
		return m
	}
	for name, tc := range map[string]struct {
		env     map[string]string
		wantErr bool
	}{
		"none":              {map[string]string{}, true},
		"valid key":         {map[string]string{envEnrollKey: testEnrollKey}, false},
		"valid client":      {client, false},
		"bad key":           {map[string]string{envEnrollKey: "bek_short"}, true},
		"key and client":    {with(client, envEnrollKey, testEnrollKey), true},
		"missing secret":    {with(client, envClientPass, ""), true},
		"missing id":        {with(client, envClientID, ""), true},
		"missing issuer":    {with(client, envIssuer, ""), true},
		"missing project":   {with(client, envProjectID, ""), true},
		"http issuer":       {with(client, envIssuer, "http://auth.example.test"), true},
		"issuer with query": {with(client, envIssuer, "https://auth.example.test?x=1"), true},
		"bad project":       {with(client, envProjectID, "a b"), true},
	} {
		if _, err := machineCredentialFromEnv(env(tc.env)); (err != nil) != tc.wantErr {
			t.Errorf("%s: err=%v wantErr=%v", name, err, tc.wantErr)
		}
	}
}

type routeTransport map[string]func(*http.Request) (int, string)

func (rt routeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	h, ok := rt[r.Method+" "+r.URL.Host+r.URL.Path]
	if !ok {
		return nil, io.ErrUnexpectedEOF
	}
	status, body := h(r)
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
}

func TestEnrollMachineWithClientSecret(t *testing.T) {
	var tokenForm url.Values
	var basic string
	var enrollAuth, enrollBody string
	rt := routeTransport{
		"GET auth.example.test/.well-known/openid-configuration": func(*http.Request) (int, string) {
			return 200, `{"issuer":"https://auth.example.test","token_endpoint":"https://auth.example.test/oauth/v2/token"}`
		},
		"POST auth.example.test/oauth/v2/token": func(r *http.Request) (int, string) {
			b, _ := io.ReadAll(r.Body)
			tokenForm, _ = url.ParseQuery(string(b))
			basic = r.Header.Get("Authorization")
			return 200, `{"access_token":"jwt-token","token_type":"Bearer","expires_in":300}`
		},
		"POST bridge.example.test/v1/device/enroll": func(r *http.Request) (int, string) {
			enrollAuth = r.Header.Get("Authorization")
			b, _ := io.ReadAll(r.Body)
			enrollBody = string(b)
			return 200, `{"bridge_url":"https://bridge.example.test","api_version":"v1","schema_version":2,"organization_id":"o","installation_id":"i","collector_credential":"brc_x"}`
		},
	}
	c, err := machineCredentialFromEnv(env(map[string]string{envClientID: "my id", envClientPass: "se/cret", envIssuer: "https://auth.example.test", envProjectID: "proj-1"}))
	if err != nil {
		t.Fatal(err)
	}
	tok, err := enrollMachine(context.Background(), &http.Client{Transport: rt}, "https://bridge.example.test", "desk-1", c)
	if err != nil || tok.InstallationID != "i" {
		t.Fatal(tok, err)
	}
	if tokenForm.Get("grant_type") != "client_credentials" || !strings.Contains(tokenForm.Get("scope"), "project:id:proj-1:aud") ||
		!strings.HasSuffix(tokenForm.Get("scope"), "role:bridge:enroll") {
		t.Fatalf("token request: %v", tokenForm)
	}
	if want := "Basic bXkraWQ6c2UlMkZjcmV0"; basic != want { // "my+id:se%2Fcret"
		t.Fatalf("client auth not form-encoded: %q", basic)
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(enrollBody), &body); err != nil {
		t.Fatal(err)
	}
	if enrollAuth != "Bearer jwt-token" || body["display_name"] != "desk-1" || len(body) != 1 {
		t.Fatalf("enroll request: %q %q", enrollAuth, enrollBody)
	}
}

func TestEnrollMachineWithKeySkipsIdentityProvider(t *testing.T) {
	var auth string
	rt := routeTransport{"POST bridge.example.test/v1/device/enroll": func(r *http.Request) (int, string) {
		auth = r.Header.Get("Authorization")
		return 200, `{"bridge_url":"https://bridge.example.test","api_version":"v1"}`
	}}
	c, _ := machineCredentialFromEnv(env(map[string]string{envEnrollKey: testEnrollKey}))
	if _, err := enrollMachine(context.Background(), &http.Client{Transport: rt}, "https://bridge.example.test", "d", c); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer "+testEnrollKey {
		t.Fatalf("auth %q", auth)
	}
}

func TestEnrollMachineFailuresDoNotLeakSecrets(t *testing.T) {
	const secret = "TOP-SECRET-VALUE"
	c := machineCredential{clientID: "id", clientSecret: secret, issuer: "https://auth.example.test", projectID: "p"}
	for name, rt := range map[string]routeTransport{
		"idp rejects": {
			"GET auth.example.test/.well-known/openid-configuration": func(*http.Request) (int, string) {
				return 200, `{"issuer":"https://auth.example.test","token_endpoint":"https://auth.example.test/t"}`
			},
			"POST auth.example.test/t": func(*http.Request) (int, string) { return 401, `{"error":"` + secret + `"}` },
		},
		"cross-origin token endpoint": {
			"GET auth.example.test/.well-known/openid-configuration": func(*http.Request) (int, string) {
				return 200, `{"issuer":"https://auth.example.test","token_endpoint":"https://evil.example/t"}`
			},
		},
		"issuer mismatch": {
			"GET auth.example.test/.well-known/openid-configuration": func(*http.Request) (int, string) {
				return 200, `{"issuer":"https://other.example","token_endpoint":"https://auth.example.test/t"}`
			},
		},
		"insecure token endpoint": {
			"GET auth.example.test/.well-known/openid-configuration": func(*http.Request) (int, string) {
				return 200, `{"issuer":"https://auth.example.test","token_endpoint":"http://auth.example.test/t"}`
			},
		},
	} {
		_, err := enrollMachine(context.Background(), &http.Client{Transport: rt}, "https://bridge.example.test", "d", c)
		if err == nil {
			t.Errorf("%s: succeeded", name)
		} else if strings.Contains(err.Error(), secret) {
			t.Errorf("%s: error leaks secret: %v", name, err)
		}
	}
	for status, want := range map[int]string{401: "refused", 403: "refused", 409: "limit", 503: "HTTP 503"} {
		rt := routeTransport{"POST bridge.example.test/v1/device/enroll": func(*http.Request) (int, string) { return status, `{}` }}
		k, _ := machineCredentialFromEnv(env(map[string]string{envEnrollKey: testEnrollKey}))
		if _, err := enrollMachine(context.Background(), &http.Client{Transport: rt}, "https://bridge.example.test", "d", k); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("status %d: %v", status, err)
		}
	}
}

func TestLoginNonInteractiveNeverFallsBackToBrowser(t *testing.T) {
	t.Setenv("BRIDGECTL_STATE_DIR", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, k := range []string{envEnrollKey, envClientID, envClientPass, envIssuer, envProjectID} {
		t.Setenv(k, "")
	}
	opened := false
	orig := openBrowser
	openBrowser = func(string) error { opened = true; return nil }
	t.Cleanup(func() { openBrowser = orig })
	originalTransport := http.DefaultTransport
	http.DefaultTransport = routeTransport{} // any network call fails the test
	t.Cleanup(func() { http.DefaultTransport = originalTransport })

	run := func(args ...string) error {
		cmd := newBridgeLoginCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(io.Discard)
		cmd.SetArgs(append([]string{"https://bridge.example.test"}, args...))
		return cmd.Execute()
	}
	if err := run("--non-interactive"); err == nil || !strings.Contains(err.Error(), envEnrollKey) {
		t.Fatalf("missing credential must fail with guidance, got %v", err)
	}
	if err := run("--non-interactive", "--organization", "Acme"); err == nil || !strings.Contains(err.Error(), "organization") {
		t.Fatalf("--organization must be rejected, got %v", err)
	}
	if opened {
		t.Fatal("--non-interactive opened a browser")
	}
}
