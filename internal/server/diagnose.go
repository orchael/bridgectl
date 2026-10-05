package server

import (
	"time"

	"github.com/orchael/bridgectl/internal/bridge"
	"github.com/orchael/bridgectl/internal/diagnose"
)

// DiagnosticReportJSON builds the canonical schema-versioned diagnostic report
// for one session snapshot, as a single JSON document. It is the one builder
// behind both the DiagnoseSession RPC and the diagnose_session control message,
// so a local caller and Bridge see the same document. stateDir locates the
// control client's status/revision files; empty means they are unknown.
func DiagnosticReportJSON(info *bridge.SessionInfo, stateDir, version string) ([]byte, error) {
	if version == "" {
		version = "dev"
	}
	in := diagnose.LoadInputs(stateDir, info.SessionID, version, time.Now())
	return diagnose.Build(sessionInfoToProto(info), in).MarshalJSON()
}
