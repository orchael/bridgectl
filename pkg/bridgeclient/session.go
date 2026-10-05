package bridgeclient

import (
	"context"

	bridgev1 "github.com/orchael/bridgectl/gen/bridge/v1"
)

func (c *Client) StartSession(ctx context.Context, req *bridgev1.StartSessionRequest) (*bridgev1.StartSessionResponse, error) {
	c.SetProject(req.ProjectId)
	var resp *bridgev1.StartSessionResponse
	err := c.invoke(ctx, func(callCtx context.Context) error {
		var callErr error
		resp, callErr = c.rpc.StartSession(callCtx, req)
		return callErr
	})
	return resp, err
}

func (c *Client) StopSession(ctx context.Context, req *bridgev1.StopSessionRequest) (*bridgev1.StopSessionResponse, error) {
	var resp *bridgev1.StopSessionResponse
	err := c.invoke(ctx, func(callCtx context.Context) error {
		var callErr error
		resp, callErr = c.rpc.StopSession(callCtx, req)
		return callErr
	})
	return resp, err
}

func (c *Client) GetSession(ctx context.Context, req *bridgev1.GetSessionRequest) (*bridgev1.GetSessionResponse, error) {
	var resp *bridgev1.GetSessionResponse
	err := c.invoke(ctx, func(callCtx context.Context) error {
		var callErr error
		resp, callErr = c.rpc.GetSession(callCtx, req)
		return callErr
	})
	return resp, err
}

// DiagnoseSession returns the daemon-built, schema-versioned diagnostic
// snapshot of a session as a single JSON document.
func (c *Client) DiagnoseSession(ctx context.Context, req *bridgev1.DiagnoseSessionRequest) (*bridgev1.DiagnoseSessionResponse, error) {
	var resp *bridgev1.DiagnoseSessionResponse
	err := c.invoke(ctx, func(callCtx context.Context) error {
		var callErr error
		resp, callErr = c.rpc.DiagnoseSession(callCtx, req)
		return callErr
	})
	return resp, err
}

func (c *Client) ListSessions(ctx context.Context, req *bridgev1.ListSessionsRequest) (*bridgev1.ListSessionsResponse, error) {
	var resp *bridgev1.ListSessionsResponse
	err := c.invoke(ctx, func(callCtx context.Context) error {
		var callErr error
		resp, callErr = c.rpc.ListSessions(callCtx, req)
		return callErr
	})
	return resp, err
}

func (c *Client) WriteInput(ctx context.Context, req *bridgev1.WriteInputRequest) (*bridgev1.WriteInputResponse, error) {
	var resp *bridgev1.WriteInputResponse
	err := c.invoke(ctx, func(callCtx context.Context) error {
		var callErr error
		resp, callErr = c.rpc.WriteInput(callCtx, req)
		return callErr
	})
	return resp, err
}

func (c *Client) ResizeSession(ctx context.Context, req *bridgev1.ResizeSessionRequest) (*bridgev1.ResizeSessionResponse, error) {
	var resp *bridgev1.ResizeSessionResponse
	err := c.invoke(ctx, func(callCtx context.Context) error {
		var callErr error
		resp, callErr = c.rpc.ResizeSession(callCtx, req)
		return callErr
	})
	return resp, err
}

func (c *Client) Health(ctx context.Context) (*bridgev1.HealthResponse, error) {
	var resp *bridgev1.HealthResponse
	err := c.invoke(ctx, func(callCtx context.Context) error {
		var callErr error
		resp, callErr = c.rpc.Health(callCtx, &bridgev1.HealthRequest{})
		return callErr
	})
	return resp, err
}

func (c *Client) ListProviders(ctx context.Context) (*bridgev1.ListProvidersResponse, error) {
	var resp *bridgev1.ListProvidersResponse
	err := c.invoke(ctx, func(callCtx context.Context) error {
		var callErr error
		resp, callErr = c.rpc.ListProviders(callCtx, &bridgev1.ListProvidersRequest{})
		return callErr
	})
	return resp, err
}

// RegisterJWTKey enrolls a client's Ed25519 public key for JWT authentication.
// Requires mTLS; JWT auth is not needed for this call.
func (c *Client) RegisterJWTKey(ctx context.Context, req *bridgev1.RegisterJWTKeyRequest) (*bridgev1.RegisterJWTKeyResponse, error) {
	var resp *bridgev1.RegisterJWTKeyResponse
	err := c.invoke(ctx, func(callCtx context.Context) error {
		var callErr error
		resp, callErr = c.rpc.RegisterJWTKey(callCtx, req)
		return callErr
	})
	return resp, err
}

func (c *Client) ClaimWriter(ctx context.Context, req *bridgev1.ClaimWriterRequest) (*bridgev1.ClaimWriterResponse, error) {
	var resp *bridgev1.ClaimWriterResponse
	err := c.invoke(ctx, func(callCtx context.Context) error {
		var callErr error
		resp, callErr = c.rpc.ClaimWriter(callCtx, req)
		return callErr
	})
	return resp, err
}

func (c *Client) ReleaseWriter(ctx context.Context, req *bridgev1.ReleaseWriterRequest) (*bridgev1.ReleaseWriterResponse, error) {
	var resp *bridgev1.ReleaseWriterResponse
	err := c.invoke(ctx, func(callCtx context.Context) error {
		var callErr error
		resp, callErr = c.rpc.ReleaseWriter(callCtx, req)
		return callErr
	})
	return resp, err
}
