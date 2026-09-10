//go:build !windows

package cli_test

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/google/uuid"
	bridgev1 "github.com/orchael/bridgectl/gen/bridge/v1"
	"github.com/orchael/bridgectl/internal/localserver"
	"github.com/orchael/bridgectl/pkg/bridgeclient"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestCLITakeover exercises PRD Human Interjection through the real CLI and SDK.
// SDK-only handoff tests cannot catch a CLI claim made before RecvAll opens the stream.
func (s *CLISuite) TestCLITakeover() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}
	stateDir := s.testStateDir()
	srv, err := localserver.Start(localserver.Config{StateDir: stateDir})
	s.Require().NoError(err)
	defer srv.Stop()
	client, err := bridgeclient.New(bridgeclient.WithTarget(srv.Target()))
	s.Require().NoError(err)
	defer func() { _ = client.Close() }()
	client.SetProject("test")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sessionID := s.startEchoSession(ctx, client, s.T().TempDir())
	oldWriter := uuid.NewString()
	_, attached, events, stopRecv := s.attachAndCollectEvents(ctx, client, sessionID, oldWriter, bridgev1.AttachRole_ATTACH_ROLE_WRITER)
	defer stopRecv()
	s.waitForAttach(attached)

	master, tty, err := pty.Open()
	s.Require().NoError(err)
	defer func() { _ = master.Close(); _ = tty.Close() }()
	outputPath := filepath.Join(s.T().TempDir(), "cli-output")
	output, err := os.Create(outputPath)
	s.Require().NoError(err)
	defer func() { _ = output.Close() }()
	cmd := exec.CommandContext(ctx, cliBinary, "session", "attach", sessionID, "--take-over")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, output, output
	s.Require().NoError(cmd.Start())
	done := make(chan struct{})
	var waitErr error
	go func() { waitErr = cmd.Wait(); close(done) }()
	defer func() { cancel(); <-done }()

	// Fail immediately if the buggy CLI exits with "claim writer: permission denied".
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
waitForWriter:
	for {
		select {
		case <-done:
			data, _ := os.ReadFile(outputPath)
			s.T().Fatalf("takeover exited before acquiring writer: %v\n%s", waitErr, data)
		case <-ctx.Done():
			s.T().Fatal("timed out waiting for CLI takeover")
		case <-ticker.C:
			info, getErr := client.GetSession(ctx, &bridgev1.GetSessionRequest{SessionId: sessionID})
			s.Require().NoError(getErr)
			if info.ActiveWriterClientId != "" && info.ActiveWriterClientId != oldWriter {
				break waitForWriter
			}
		}
	}
	_, err = master.Write([]byte("takeover-input-marker\n"))
	s.Require().NoError(err)
	s.Require().Eventually(func() bool {
		var output strings.Builder
		for _, ev := range events() {
			if ev.Type == bridgev1.AttachEventType_ATTACH_EVENT_TYPE_OUTPUT {
				output.Write(ev.Payload)
			}
		}
		return strings.Contains(output.String(), "takeover-input-marker")
	}, 5*time.Second, 10*time.Millisecond, "old writer must remain an observer and receive CLI input output")
	s.Assert().True(hasEventType(events(), bridgev1.AttachEventType_ATTACH_EVENT_TYPE_WRITER_RELEASED))
	s.Assert().True(hasEventType(events(), bridgev1.AttachEventType_ATTACH_EVENT_TYPE_WRITER_CLAIMED))

	_, err = master.Write([]byte{0x1d}) // Ctrl-]
	s.Require().NoError(err)
	select {
	case <-done:
		s.Require().NoError(waitErr)
	case <-ctx.Done():
		s.T().Fatal("CLI did not detach")
	}
	info, err := client.GetSession(ctx, &bridgev1.GetSessionRequest{SessionId: sessionID})
	s.Require().NoError(err)
	s.Assert().Empty(info.ActiveWriterClientId)
}

type rejectingTakeoverServer struct {
	bridgev1.UnimplementedBridgeServiceServer
	claimStarted chan *bridgev1.ClaimWriterRequest
	rejectClaim  chan struct{}
	earlyInput   chan string
	attached     chan *bridgev1.AttachSessionRequest
}

func (s *CLISuite) TestCLITakeoverMissingSession() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}
	srv, err := localserver.Start(localserver.Config{StateDir: s.testStateDir()})
	s.Require().NoError(err)
	defer srv.Stop()
	master, tty, err := pty.Open()
	s.Require().NoError(err)
	defer func() { _ = master.Close(); _ = tty.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, cliBinary, "session", "attach", uuid.NewString(), "--take-over")
	cmd.Stdin = tty
	output, err := cmd.CombinedOutput()
	s.Require().Error(err, "failed attachment must return a command error: %s", output)
	s.Require().NoError(ctx.Err(), "CLI must exit without waiting for timeout")
	s.Assert().Contains(string(output), "session not found")
}

func (s *rejectingTakeoverServer) Health(context.Context, *bridgev1.HealthRequest) (*bridgev1.HealthResponse, error) {
	return &bridgev1.HealthResponse{Status: "serving"}, nil
}

func (s *rejectingTakeoverServer) AttachSession(req *bridgev1.AttachSessionRequest, stream grpc.ServerStreamingServer[bridgev1.AttachSessionEvent]) error {
	s.attached <- req
	if err := stream.Send(&bridgev1.AttachSessionEvent{Type: bridgev1.AttachEventType_ATTACH_EVENT_TYPE_ATTACHED}); err != nil {
		return err
	}
	<-stream.Context().Done()
	return stream.Context().Err()
}

func (s *rejectingTakeoverServer) ClaimWriter(ctx context.Context, req *bridgev1.ClaimWriterRequest) (*bridgev1.ClaimWriterResponse, error) {
	s.claimStarted <- req
	select {
	case <-s.rejectClaim:
		return nil, status.Error(codes.PermissionDenied, "test claim rejected")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *rejectingTakeoverServer) WriteInput(context.Context, *bridgev1.WriteInputRequest) (*bridgev1.WriteInputResponse, error) {
	s.earlyInput <- "input"
	return &bridgev1.WriteInputResponse{}, nil
}

func (s *rejectingTakeoverServer) ResizeSession(context.Context, *bridgev1.ResizeSessionRequest) (*bridgev1.ResizeSessionResponse, error) {
	s.earlyInput <- "resize"
	return &bridgev1.ResizeSessionResponse{}, nil
}

// A rejected takeover must exit unsuccessfully and never forward input or resize
// while the claim is pending, even if the terminal already has input queued.
func (s *CLISuite) TestCLITakeoverClaimFailure() {
	if testing.Short() {
		s.T().Skip("skipping in short mode")
	}
	stateDir := s.testStateDir()
	listener, err := net.Listen("unix", filepath.Join(stateDir, "server.sock"))
	s.Require().NoError(err)
	rpc := &rejectingTakeoverServer{
		claimStarted: make(chan *bridgev1.ClaimWriterRequest, 1),
		rejectClaim:  make(chan struct{}),
		earlyInput:   make(chan string, 16),
		attached:     make(chan *bridgev1.AttachSessionRequest, 1),
	}
	server := grpc.NewServer()
	bridgev1.RegisterBridgeServiceServer(server, rpc)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()

	master, tty, err := pty.Open()
	s.Require().NoError(err)
	defer func() { _ = master.Close(); _ = tty.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, cliBinary, "session", "attach", uuid.NewString(), "--take-over")
	cmd.Stdin = tty
	var output strings.Builder
	cmd.Stdout, cmd.Stderr = &output, &output
	s.Require().NoError(cmd.Start())
	done := make(chan struct{})
	var waitErr error
	go func() { waitErr = cmd.Wait(); close(done) }()
	defer func() { cancel(); <-done }()
	select {
	case claim := <-rpc.claimStarted:
		s.Assert().True(claim.Force)
		select {
		case attach := <-rpc.attached:
			s.Assert().Equal(bridgev1.AttachRole_ATTACH_ROLE_OBSERVER, attach.Role)
			s.Assert().Equal(attach.ClientId, claim.ClientId)
			s.Assert().Equal(attach.SessionId, claim.SessionId)
		default:
			s.T().Fatal("claim was sent before attachment")
		}
	case <-ctx.Done():
		s.T().Fatal("CLI never claimed writer")
	case <-done:
		s.T().Fatalf("CLI exited before claiming writer: %v\n%s", waitErr, output.String())
	}
	_, err = master.Write([]byte("must-not-forward\n"))
	s.Require().NoError(err)
	s.Require().NoError(cmd.Process.Signal(syscall.SIGWINCH))
	select {
	case operation := <-rpc.earlyInput:
		s.T().Fatalf("CLI sent %s before claim succeeded", operation)
	case <-time.After(100 * time.Millisecond):
	}
	close(rpc.rejectClaim)
	select {
	case <-done:
		s.Require().Error(waitErr)
		s.Assert().Contains(output.String(), "claim writer: permission denied")
	case <-ctx.Done():
		s.T().Fatal("CLI did not exit after rejected claim")
	}
	s.Assert().Empty(rpc.earlyInput, "failed claim must not enable terminal forwarding")
}
