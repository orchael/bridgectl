package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	bridgev1 "github.com/orchael/bridgectl/gen/bridge/v1"
	"github.com/orchael/bridgectl/internal/diagnose"
	"github.com/orchael/bridgectl/internal/localserver"
	"github.com/orchael/bridgectl/pkg/bridgeclient"
)

// sessionGetter fetches one session through the local server's public
// GetSession API. It is a seam for tests; production dials the local server.
type sessionGetter func(ctx context.Context, sessionID string) (*bridgev1.GetSessionResponse, error)

func newSessionDiagnoseCmd() *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "diagnose <session-id>",
		Short: "Report what bridgectl currently believes about a session",
		Long: "Report bridgectl's current authoritative view of a local session: runtime status, " +
			"interaction state and capability, revisions, pending request, writer presence and " +
			"local Bridge control status.\n\n" +
			"This is a snapshot of current state, not an event replay. It contains no terminal output, " +
			"prompts, responses, environment variables, credentials or filesystem contents. " +
			"Use --json for the versioned (schema_version 1) machine-readable form.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSessionDiagnose(cmd.Context(), cmd.OutOrStdout(), args[0], jsonOutput, getSessionLocal, time.Now())
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit the schema-versioned diagnostic as a single JSON object")
	return cmd
}

func getSessionLocal(ctx context.Context, sessionID string) (*bridgev1.GetSessionResponse, error) {
	client, err := connectClient("", 5*time.Second)
	if err != nil {
		return nil, errServerUnavailable
	}
	defer func() { _ = client.Close() }()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return client.GetSession(ctx, &bridgev1.GetSessionRequest{SessionId: sessionID})
}

var errServerUnavailable = errors.New("local bridgectl server unavailable")

// runSessionDiagnose is the whole command: fetch via the public session API,
// build the canonical Report, then serialize it (--json) or render it. On
// failure it emits a fixed-vocabulary error (a JSON error object under
// --json) and returns a non-zero-exit error; it never echoes server error
// text, which could carry provider output.
func runSessionDiagnose(ctx context.Context, out io.Writer, sessionID string, jsonOutput bool, get sessionGetter, now time.Time) error {
	fail := func(code diagnose.ErrorCode) error {
		rep := diagnose.NewErrorReport(code)
		if jsonOutput {
			if err := json.NewEncoder(out).Encode(rep); err != nil {
				return err
			}
		}
		return fmt.Errorf("session diagnose: %s", rep.Error.Message)
	}

	if _, err := uuid.Parse(sessionID); err != nil {
		return fail(diagnose.CodeInvalidSessionID)
	}
	resp, err := get(ctx, sessionID)
	switch {
	case err == nil:
	case errors.Is(err, bridgeclient.ErrSessionNotFound):
		return fail(diagnose.CodeSessionNotFound)
	case errors.Is(err, errServerUnavailable), isUnavailable(err):
		return fail(diagnose.CodeServerUnavailable)
	default:
		return fail(diagnose.CodeInternal)
	}

	rep := diagnose.Build(resp, diagnose.LoadInputs(localserver.StateDir(), sessionID, version, now))
	if jsonOutput {
		b, err := rep.MarshalJSON()
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "%s\n", b)
		return err
	}
	diagnose.Render(out, rep)
	return nil
}

func isUnavailable(err error) bool {
	c := status.Code(err)
	return c == codes.Unavailable || c == codes.DeadlineExceeded
}
