package bridgeclient

import (
	"context"
	"errors"
	"testing"
	"time"

	bridgev1 "github.com/orchael/bridgectl/gen/bridge/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type fakeRPCClient struct {
	startResp     *bridgev1.StartSessionResponse
	stopResp      *bridgev1.StopSessionResponse
	getResp       *bridgev1.GetSessionResponse
	listResp      *bridgev1.ListSessionsResponse
	writeResp     *bridgev1.WriteInputResponse
	resizeResp    *bridgev1.ResizeSessionResponse
	healthResp    *bridgev1.HealthResponse
	providersResp *bridgev1.ListProvidersResponse
	diagnoseResp  *bridgev1.DiagnoseSessionResponse
	err           error
}

func (f *fakeRPCClient) StartSession(context.Context, *bridgev1.StartSessionRequest, ...grpc.CallOption) (*bridgev1.StartSessionResponse, error) {
	return f.startResp, f.err
}
func (f *fakeRPCClient) StopSession(context.Context, *bridgev1.StopSessionRequest, ...grpc.CallOption) (*bridgev1.StopSessionResponse, error) {
	return f.stopResp, f.err
}
func (f *fakeRPCClient) GetSession(context.Context, *bridgev1.GetSessionRequest, ...grpc.CallOption) (*bridgev1.GetSessionResponse, error) {
	return f.getResp, f.err
}
func (f *fakeRPCClient) DiagnoseSession(context.Context, *bridgev1.DiagnoseSessionRequest, ...grpc.CallOption) (*bridgev1.DiagnoseSessionResponse, error) {
	return f.diagnoseResp, f.err
}
func (f *fakeRPCClient) ListSessions(context.Context, *bridgev1.ListSessionsRequest, ...grpc.CallOption) (*bridgev1.ListSessionsResponse, error) {
	return f.listResp, f.err
}
func (f *fakeRPCClient) AttachSession(context.Context, *bridgev1.AttachSessionRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[bridgev1.AttachSessionEvent], error) {
	return nil, f.err
}
func (f *fakeRPCClient) WriteInput(context.Context, *bridgev1.WriteInputRequest, ...grpc.CallOption) (*bridgev1.WriteInputResponse, error) {
	return f.writeResp, f.err
}
func (f *fakeRPCClient) ResizeSession(context.Context, *bridgev1.ResizeSessionRequest, ...grpc.CallOption) (*bridgev1.ResizeSessionResponse, error) {
	return f.resizeResp, f.err
}
func (f *fakeRPCClient) Health(context.Context, *bridgev1.HealthRequest, ...grpc.CallOption) (*bridgev1.HealthResponse, error) {
	return f.healthResp, f.err
}
func (f *fakeRPCClient) ListProviders(context.Context, *bridgev1.ListProvidersRequest, ...grpc.CallOption) (*bridgev1.ListProvidersResponse, error) {
	return f.providersResp, f.err
}
func (f *fakeRPCClient) ClaimWriter(context.Context, *bridgev1.ClaimWriterRequest, ...grpc.CallOption) (*bridgev1.ClaimWriterResponse, error) {
	return nil, f.err
}
func (f *fakeRPCClient) ReleaseWriter(context.Context, *bridgev1.ReleaseWriterRequest, ...grpc.CallOption) (*bridgev1.ReleaseWriterResponse, error) {
	return nil, f.err
}
func (f *fakeRPCClient) RegisterJWTKey(context.Context, *bridgev1.RegisterJWTKeyRequest, ...grpc.CallOption) (*bridgev1.RegisterJWTKeyResponse, error) {
	return nil, f.err
}
func (f *fakeRPCClient) EnrollClient(context.Context, *bridgev1.EnrollClientRequest, ...grpc.CallOption) (*bridgev1.EnrollClientResponse, error) {
	return nil, f.err
}

func TestClientSessionMethods(t *testing.T) {
	c := &Client{
		rpc:     &fakeRPCClient{},
		retry:   RetryConfig{MaxAttempts: 1},
		timeout: time.Second,
	}

	fake := c.rpc.(*fakeRPCClient)
	fake.startResp = &bridgev1.StartSessionResponse{SessionId: "session-a"}
	startResp, err := c.StartSession(context.Background(), &bridgev1.StartSessionRequest{ProjectId: "project-a"})
	if err != nil || startResp.GetSessionId() != "session-a" {
		t.Fatalf("StartSession resp=%+v err=%v", startResp, err)
	}

	fake.stopResp = &bridgev1.StopSessionResponse{Status: bridgev1.SessionStatus_SESSION_STATUS_STOPPING}
	stopResp, err := c.StopSession(context.Background(), &bridgev1.StopSessionRequest{})
	if err != nil || stopResp.GetStatus() != bridgev1.SessionStatus_SESSION_STATUS_STOPPING {
		t.Fatalf("StopSession resp=%+v err=%v", stopResp, err)
	}

	fake.getResp = &bridgev1.GetSessionResponse{SessionId: "session-a"}
	getResp, err := c.GetSession(context.Background(), &bridgev1.GetSessionRequest{})
	if err != nil || getResp.GetSessionId() != "session-a" {
		t.Fatalf("GetSession resp=%+v err=%v", getResp, err)
	}

	fake.diagnoseResp = &bridgev1.DiagnoseSessionResponse{ReportJson: []byte(`{"schema_version":1}`)}
	diagResp, err := c.DiagnoseSession(context.Background(), &bridgev1.DiagnoseSessionRequest{SessionId: "session-a"})
	if err != nil || string(diagResp.GetReportJson()) != `{"schema_version":1}` {
		t.Fatalf("DiagnoseSession resp=%+v err=%v", diagResp, err)
	}

	fake.listResp = &bridgev1.ListSessionsResponse{Sessions: []*bridgev1.GetSessionResponse{{SessionId: "session-a"}}}
	listResp, err := c.ListSessions(context.Background(), &bridgev1.ListSessionsRequest{})
	if err != nil || len(listResp.GetSessions()) != 1 {
		t.Fatalf("ListSessions resp=%+v err=%v", listResp, err)
	}

	fake.writeResp = &bridgev1.WriteInputResponse{Accepted: true, BytesWritten: 5}
	writeResp, err := c.WriteInput(context.Background(), &bridgev1.WriteInputRequest{})
	if err != nil || !writeResp.GetAccepted() {
		t.Fatalf("WriteInput resp=%+v err=%v", writeResp, err)
	}

	fake.resizeResp = &bridgev1.ResizeSessionResponse{Applied: true}
	resizeResp, err := c.ResizeSession(context.Background(), &bridgev1.ResizeSessionRequest{})
	if err != nil || !resizeResp.GetApplied() {
		t.Fatalf("ResizeSession resp=%+v err=%v", resizeResp, err)
	}

	fake.healthResp = &bridgev1.HealthResponse{Status: "serving"}
	healthResp, err := c.Health(context.Background())
	if err != nil || healthResp.GetStatus() != "serving" {
		t.Fatalf("Health resp=%+v err=%v", healthResp, err)
	}

	fake.providersResp = &bridgev1.ListProvidersResponse{Providers: []*bridgev1.ProviderInfo{{Provider: "fake"}}}
	providersResp, err := c.ListProviders(context.Background())
	if err != nil || len(providersResp.GetProviders()) != 1 {
		t.Fatalf("ListProviders resp=%+v err=%v", providersResp, err)
	}
}

func TestInvokeRetriesAndMapsErrors(t *testing.T) {
	c := &Client{
		retry: RetryConfig{
			MaxAttempts:    2,
			InitialBackoff: time.Millisecond,
			MaxBackoff:     time.Millisecond,
		},
		timeout: time.Second,
	}

	attempts := 0
	err := c.invoke(context.Background(), func(context.Context) error {
		attempts++
		if attempts == 1 {
			return status.Error(codes.Unavailable, "retry me")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("invoke retry err=%v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts=%d want 2", attempts)
	}

	err = c.invoke(context.Background(), func(context.Context) error {
		return errors.New("boom")
	})
	if err == nil || err.Error() != "boom" {
		t.Fatalf("invoke error=%v want raw boom error", err)
	}
}
