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

// reportFetcher obtains the diagnostic Report for a session. It is a seam for
// tests; production asks the local daemon.
type reportFetcher func(ctx context.Context, sessionID string) (*diagnose.Report, error)

func newSessionDiagnoseCmd() *cobra.Command {
	var (
		jsonOutput                         bool
		remote, cert, key, jwtKey, srvName string
	)
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
			fetch := func(ctx context.Context, id string) (*diagnose.Report, error) {
				return fetchReportFrom(ctx, id, remote, cert, key, jwtKey, srvName)
			}
			return runSessionDiagnose(cmd.Context(), cmd.OutOrStdout(), args[0], jsonOutput, fetch)
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit the schema-versioned diagnostic as a single JSON object")
	addRemoteFlags(cmd, &remote, &cert, &key, &jwtKey, &srvName)
	return cmd
}

// fetchReportFrom asks the local daemon, or the --remote one, for the report.
// The daemon builds it from its own state, so a remote report describes the
// remote machine's control status and revisions, not this one's.
func fetchReportFrom(ctx context.Context, sessionID, remote, cert, key, jwtKey, serverName string) (*diagnose.Report, error) {
	client, err := connectClientForHost(remote, 5*time.Second, cert, key, jwtKey, serverName)
	if err != nil {
		return nil, errServerUnavailable
	}
	defer func() { _ = client.Close() }()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return fetchReport(ctx, sessionID,
		func(ctx context.Context, id string) ([]byte, error) {
			resp, err := client.DiagnoseSession(ctx, &bridgev1.DiagnoseSessionRequest{SessionId: id})
			if err != nil {
				return nil, err
			}
			return resp.GetReportJson(), nil
		},
		func(ctx context.Context) string {
			resp, err := client.Health(ctx)
			if err != nil || resp.GetServerVersion() == "" {
				return diagnose.UnknownVersion
			}
			return resp.GetServerVersion()
		},
		func(ctx context.Context, id string) (*bridgev1.GetSessionResponse, error) {
			if remote != "" {
				// Building from GetSession would mix a remote session with
				// this machine's control files.
				return nil, errors.New("remote bridgectl server does not support DiagnoseSession")
			}
			return client.GetSession(ctx, &bridgev1.GetSessionRequest{SessionId: id})
		})
}

// fetchReport asks the daemon to build the report (DiagnoseSession). A daemon
// that predates that RPC answers Unimplemented — common while an older server
// is still running after an upgrade — in which case the same Report is built
// locally from GetSession plus this machine's control files, stamped with the
// daemon's version from Health ("unknown" if unavailable).
func fetchReport(ctx context.Context, sessionID string,
	diagnoseRPC func(context.Context, string) ([]byte, error),
	daemonVersion func(context.Context) string,
	getSession func(context.Context, string) (*bridgev1.GetSessionResponse, error),
) (*diagnose.Report, error) {
	raw, err := diagnoseRPC(ctx, sessionID)
	switch {
	case err == nil:
		var rep diagnose.Report
		if uerr := json.Unmarshal(raw, &rep); uerr != nil || rep.SchemaVersion != diagnose.SchemaVersion {
			return nil, errors.New("unsupported diagnostic response from server")
		}
		return &rep, nil
	case status.Code(err) != codes.Unimplemented:
		return nil, err
	}
	resp, err := getSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	// The report describes the daemon, so name the daemon's version, not this
	// CLI's: during an upgrade without a restart the mismatch is exactly what
	// diagnostics should reveal.
	return diagnose.Build(resp, diagnose.LoadInputs(localserver.StateDir(), sessionID, daemonVersion(ctx), time.Now())), nil
}

var errServerUnavailable = errors.New("local bridgectl server unavailable")

// runSessionDiagnose is the whole command: fetch the canonical Report, then
// serialize it (--json) or render it. On failure it emits a fixed-vocabulary
// error (a JSON error object under --json) and returns a non-zero-exit error;
// it never echoes server error text, which could carry provider output.
func runSessionDiagnose(ctx context.Context, out io.Writer, sessionID string, jsonOutput bool, fetch reportFetcher) error {
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
	rep, err := fetch(ctx, sessionID)
	switch {
	case err == nil:
	case errors.Is(err, bridgeclient.ErrSessionNotFound):
		return fail(diagnose.CodeSessionNotFound)
	case errors.Is(err, errServerUnavailable), isUnavailable(err):
		return fail(diagnose.CodeServerUnavailable)
	default:
		return fail(diagnose.CodeInternal)
	}

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
