package diagnose

import (
	"fmt"
	"io"
)

// Render writes the concise human-readable form of r. It only formats fields
// already present in the Report; it computes no state of its own.
func Render(w io.Writer, r *Report) {
	p := func(label, value string) { _, _ = fmt.Fprintf(w, "  %-26s %s\n", label, value) }

	_, _ = fmt.Fprintf(w, "Session %s (schema v%d, bridgectl %s)\n", r.SessionID, r.SchemaVersion, r.BridgectlVersion)
	p("provider", r.Provider)
	p("project", r.ProjectID)
	p("created", timeOr(r.CreatedAt, "not reported"))

	_, _ = fmt.Fprintln(w, "Runtime")
	p("status", r.Status)
	if r.ExitCode != nil {
		p("exit code", fmt.Sprintf("%d", *r.ExitCode))
	}
	if r.StoppedAt != nil {
		p("stopped", *r.StoppedAt)
	}
	p("active writer", yesNo(r.ActiveWriter))
	p("observers", fmt.Sprintf("%d", r.ObserverCount))

	_, _ = fmt.Fprintln(w, "Interaction")
	p("state", r.InteractionState)
	if c := r.InteractionCapability; c == nil {
		p("capability", "unknown (not reported)")
	} else {
		p("capability", fmt.Sprintf("state=%s approval=%s summary=%s remote_response=%s structured_approval=%s",
			yesNo(c.InteractionStateSupported), yesNo(c.ApprovalStateSupported), yesNo(c.PendingSummarySupported),
			yesNo(c.RemoteResponseSupported), yesNo(c.StructuredApprovalSupported)))
	}
	if pr := r.PendingRequest; pr == nil {
		p("pending request", "none")
	} else {
		p("pending request", fmt.Sprintf("%s (%s)", pr.ID, pr.Type))
		p("pending summary", yesNo(pr.SummaryAvailable)+" (text never included)")
	}
	p("updated", timeOr(r.InteractionUpdatedAt, "not reported"))
	p("last report", timeOr(r.InteractionLastReportAt, "not reported"))

	_, _ = fmt.Fprintln(w, "Revisions")
	p("lifecycle (wire)", numOr(r.LifecycleRevisionWire))
	p("interaction (wire)", numOr(r.InteractionRevisionWire))
	p("interaction (local)", numOr(r.InteractionRevisionLocal))

	_, _ = fmt.Fprintln(w, "Bridge control (local status file)")
	state := r.Control.State
	if r.Control.Stale {
		state += " (stale)"
	}
	p("state", state)
	p("updated", timeOr(r.Control.UpdatedAt, "not reported"))
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func timeOr(s *string, fallback string) string {
	if s == nil {
		return fallback
	}
	return *s
}

func numOr[T int64 | uint64](n *T) string {
	if n == nil {
		return "unknown"
	}
	return fmt.Sprintf("%d", *n)
}
