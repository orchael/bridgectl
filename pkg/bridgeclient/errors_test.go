package bridgeclient

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func grpcErr(code codes.Code, msg string) error {
	return status.Error(code, msg)
}

func TestMapError_Nil(t *testing.T) {
	if got := mapError(nil); got != nil {
		t.Fatalf("mapError(nil) = %v, want nil", got)
	}
}

func TestMapError_NotFound(t *testing.T) {
	err := mapError(grpcErr(codes.NotFound, "session not found"))
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("want ErrSessionNotFound, got %v", err)
	}
}

func TestMapError_AlreadyExists(t *testing.T) {
	err := mapError(grpcErr(codes.AlreadyExists, "exists"))
	if !errors.Is(err, ErrSessionAlreadyExists) {
		t.Fatalf("want ErrSessionAlreadyExists, got %v", err)
	}
}

func TestMapError_Unauthenticated(t *testing.T) {
	err := mapError(grpcErr(codes.Unauthenticated, "bad token"))
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized, got %v", err)
	}
}

func TestMapError_PermissionDenied(t *testing.T) {
	err := mapError(grpcErr(codes.PermissionDenied, "denied"))
	if !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("want ErrPermissionDenied, got %v", err)
	}
}

func TestMapError_ResourceExhausted_RateLimit(t *testing.T) {
	err := mapError(grpcErr(codes.ResourceExhausted, "rate limit exceeded"))
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("want ErrRateLimited, got %v", err)
	}
}

func TestMapError_ResourceExhausted_SessionLimit(t *testing.T) {
	err := mapError(grpcErr(codes.ResourceExhausted, "session limit reached"))
	if !errors.Is(err, ErrSessionLimitReached) {
		t.Fatalf("want ErrSessionLimitReached, got %v", err)
	}
}

// codes.Unavailable is always passed through — both transport failures (TLS,
// connection refused, DNS, timeout, "all SubConns are in TransientFailure", …)
// and server-side "provider unavailable" arrive with this code. Callers need
// the original message to diagnose the problem.
func TestMapError_Unavailable_PassesThrough(t *testing.T) {
	cases := []string{
		"provider down",
		"transport: authentication handshake failed: tls: failed to verify certificate: x509: certificate signed by unknown authority",
		`connection error: desc = "transport: Error while dialing: dial tcp 127.0.0.1:9445: connect: connection refused"`,
		`connection error: desc = "transport: Error while dialing: dial tcp: lookup macbook.ts.net: no such host"`,
		`connection error: desc = "transport: Error while dialing: dial tcp macbook.ts.net:9445: i/o timeout"`,
		`connection error: desc = "something unexpected happened"`,
		"all SubConns are in TransientFailure",
	}
	for _, msg := range cases {
		err := mapError(grpcErr(codes.Unavailable, msg))
		if err == nil {
			t.Fatalf("mapError(Unavailable, %q) = nil, want non-nil", msg)
		}
		if errors.Is(err, ErrProviderUnavailable) {
			t.Fatalf("mapError(Unavailable, %q) returned ErrProviderUnavailable sentinel; want raw gRPC error", msg)
		}
	}
}

func TestMapError_Unknown_PassThrough(t *testing.T) {
	orig := grpcErr(codes.Internal, "internal error")
	err := mapError(orig)
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	// Unknown gRPC codes should pass through as-is (not wrapped into a sentinel).
	if errors.Is(err, ErrSessionNotFound) || errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("unexpected sentinel: %v", err)
	}
}

func TestMapError_NonGRPC(t *testing.T) {
	orig := errors.New("plain error")
	err := mapError(orig)
	if err != orig {
		t.Fatalf("non-gRPC error should pass through unchanged, got %v", err)
	}
}

// TestMapError_SentinelMessages verifies that each mapped sentinel carries the
// expected human-readable message so callers that only read err.Error() still
// get a useful string without having to know the sentinel variable name.
func TestMapError_SentinelMessages(t *testing.T) {
	cases := []struct {
		code    codes.Code
		grpcMsg string
		wantMsg string
	}{
		{codes.NotFound, "session not found", "session not found"},
		{codes.AlreadyExists, "exists", "session already exists"},
		{codes.Unauthenticated, "bad token", "unauthorized"},
		{codes.PermissionDenied, "denied", "permission denied"},
		{codes.ResourceExhausted, "rate limit exceeded", "rate limited"},
		{codes.ResourceExhausted, "session limit reached", "session limit reached"},
	}
	for _, tc := range cases {
		err := mapError(grpcErr(tc.code, tc.grpcMsg))
		if err == nil {
			t.Errorf("code=%v msg=%q: mapError returned nil", tc.code, tc.grpcMsg)
			continue
		}
		if err.Error() != tc.wantMsg {
			t.Errorf("code=%v msg=%q: Error()=%q, want %q", tc.code, tc.grpcMsg, err.Error(), tc.wantMsg)
		}
	}
}

// TestMapError_PassThroughPreservesMessage confirms that errors passed through
// unchanged (Unavailable, Internal, etc.) still carry the original gRPC message
// so callers can present a useful diagnostic.
func TestMapError_PassThroughPreservesMessage(t *testing.T) {
	msg := "connection error: dial tcp 127.0.0.1:9445: connect: connection refused"
	err := mapError(grpcErr(codes.Unavailable, msg))
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	if !strings.Contains(err.Error(), msg) {
		t.Errorf("pass-through error message %q does not contain original message %q", err.Error(), msg)
	}
}

// TestSentinelErrors verifies that the public sentinel error variables have the
// expected message strings.
func TestSentinelErrors(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{ErrSessionNotFound, "session not found"},
		{ErrSessionAlreadyExists, "session already exists"},
		{ErrProviderUnavailable, "provider unavailable"},
		{ErrUnauthorized, "unauthorized"},
		{ErrPermissionDenied, "permission denied"},
		{ErrInputTooLarge, "input too large"},
		{ErrSessionLimitReached, "session limit reached"},
		{ErrRateLimited, "rate limited"},
	}
	for _, tc := range cases {
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("%v.Error() = %q, want %q", tc.err, got, tc.want)
		}
	}
}
