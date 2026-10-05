package localserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/orchael/bridgectl/internal/bridge"
	"github.com/orchael/bridgectl/internal/bridgecontrol"
	"github.com/orchael/bridgectl/internal/config"
	"github.com/orchael/bridgectl/internal/telemetry"
)

// Enrollment reload is a same-user, file-based management handshake. It also
// works for TLS daemons without adding a remotely callable configuration API.
// Requests are written only after login has committed all enrollment files.
type enrollmentReload struct {
	ID         string `json:"id"`
	ConfigPath string `json:"config_path,omitempty"`
	Error      string `json:"error,omitempty"`
}

func writeEnrollmentReload(path string, value enrollmentReload) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".enrollment-reload-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func readEnrollmentReload(path string) (enrollmentReload, error) {
	data, err := SecureReadFile(path)
	if err != nil {
		return enrollmentReload{}, err
	}
	var request enrollmentReload
	err = json.Unmarshal(data, &request)
	return request, err
}

// ReloadEnrollment waits for the running daemon to apply saved reporting
// configuration. It never restarts the daemon or interrupts provider sessions.
func ReloadEnrollment(ctx context.Context, stateDir, configPath string) error {
	path, err := filepath.Abs(configPath)
	if err != nil {
		return err
	}
	request := enrollmentReload{ID: generateInstanceID(), ConfigPath: path}
	requestPath := filepath.Join(stateDir, "enrollment-reload.json")
	if err := writeEnrollmentReload(requestPath, request); err != nil {
		return err
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		ack, err := readEnrollmentReload(filepath.Join(stateDir, "enrollment-reload-result.json"))
		if err == nil && ack.ID == request.ID {
			if ack.Error != "" {
				return errors.New(ack.Error)
			}
			return nil
		}
		if current, err := readEnrollmentReload(requestPath); err == nil && current.ID != request.ID {
			return errors.New("enrollment reload superseded by another login")
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("daemon did not acknowledge enrollment reload (an older daemon may need restarting): %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// reportingObservers keeps Supervisor's observers stable while swapping their
// destinations. Network shutdown never holds the forwarding mutex.
type reportingObservers struct {
	mu        sync.Mutex
	telemetry bridge.TelemetryObserver
	control   bridge.ControlObserver
}

func (r *reportingObservers) SessionChanged(info bridge.SessionInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.control != nil {
		r.control.SessionChanged(info)
	}
}
func (r *reportingObservers) SessionStarted(session telemetry.Session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.telemetry != nil {
		r.telemetry.SessionStarted(session)
	}
}
func (r *reportingObservers) SessionEnded(session telemetry.Session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.telemetry != nil {
		r.telemetry.SessionEnded(session)
	}
}
func (r *reportingObservers) ObserveProviderChunk(session telemetry.Session, stream telemetry.StreamType, data []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.telemetry != nil {
		r.telemetry.ObserveProviderChunk(session, stream, data)
	}
}
func (r *reportingObservers) ObserveInputChunk(session telemetry.Session, data []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.telemetry != nil {
		r.telemetry.ObserveInputChunk(session, data)
	}
}
func (r *reportingObservers) Close(ctx context.Context) error {
	r.mu.Lock()
	collector, control := r.telemetry, r.control
	r.telemetry, r.control = nil, nil
	r.mu.Unlock()
	var err error
	if control != nil {
		err = control.Close(ctx)
	}
	if collector != nil {
		err = errors.Join(err, collector.Close(ctx))
	}
	return err
}

func (s *Server) reloadEnrollment(ctx context.Context, reporting *reportingObservers, configPath, version string, historyLoaded bool) error {
	if !historyLoaded {
		return errors.New("session history failed to load; enrollment cannot publish an incomplete snapshot")
	}
	if !filepath.IsAbs(configPath) {
		return errors.New("enrollment config path must be absolute")
	}
	fileCfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load enrollment configuration: %w", err)
	}
	if err := reporting.Close(ctx); err != nil {
		return fmt.Errorf("close previous reporting clients: %w", err)
	}
	collector, err := newTelemetryObserver(fileCfg.Telemetry, s.stateDir, version, s.logger)
	if err != nil {
		return err
	}
	control, err := newControlClient(fileCfg.Control, s.stateDir, version, s.logger, s.supervisor)
	if err != nil {
		if collector != nil {
			_ = collector.Close(ctx)
		}
		return err
	}
	// Seed telemetry for sessions that were started before enrollment. Control
	// sends its own authoritative snapshot after completing its handshake.
	if collector != nil {
		for _, info := range s.supervisor.List("") {
			if info.State != bridge.SessionStateStopped && info.State != bridge.SessionStateFailed {
				collector.SessionStarted(telemetry.Session{SessionID: info.SessionID, ProjectID: info.ProjectID, Provider: info.Provider, RepoPath: info.RepoPath})
			}
		}
	}
	reporting.mu.Lock()
	reporting.telemetry = collector
	if control != nil {
		reporting.control = bridgecontrol.NewSupervisorObserver(control)
	}
	reporting.mu.Unlock()
	if control != nil {
		// Start only schedules run()'s goroutine; it returns before any
		// handshake happens. Without a synchronous write here, the status
		// file can still hold a "connected" (or "auth_rejected") entry from
		// a previous connection at the moment this reload's ack is
		// written, and waitForBridgeControl could report success — or a
		// stale rejection — without this client ever attempting its own
		// handshake.
		if err := bridgecontrol.WriteStatus(filepath.Join(s.stateDir, bridgecontrol.StatusFileName), bridgecontrol.Status{State: bridgecontrol.StateConnecting, UpdatedAt: time.Now(), LastConnectedAt: previousLastConnected(s.stateDir)}); err != nil {
			s.logger.Warn("write bridge control status", "error", err)
		}
		control.Start(context.Background())
	} else {
		_ = bridgecontrol.WriteStatus(filepath.Join(s.stateDir, bridgecontrol.StatusFileName), bridgecontrol.Status{State: bridgecontrol.StateNotProvisioned, UpdatedAt: time.Now(), LastConnectedAt: previousLastConnected(s.stateDir)})
	}
	return nil
}

// previousLastConnected returns the last-successful-connection time recorded
// in the existing status file, so a reload's synchronous status write does not
// erase it before the new control client can carry it forward.
func previousLastConnected(stateDir string) time.Time {
	if prev, err := bridgecontrol.ReadStatus(filepath.Join(stateDir, bridgecontrol.StatusFileName)); err == nil {
		return prev.LastConnectedAt
	}
	return time.Time{}
}

func (s *Server) startEnrollmentReload(reporting *reportingObservers, version string, historyLoaded bool) (context.CancelFunc, <-chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	path := filepath.Join(s.stateDir, "enrollment-reload.json")
	// Do not replay a prior process's request over this daemon's startup config.
	previous, _ := readEnrollmentReload(path)
	go func() {
		defer close(done)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			request, err := readEnrollmentReload(path)
			if err != nil || request.ID == "" || request.ID == previous.ID {
				continue
			}
			previous = request
			ack := enrollmentReload{ID: request.ID}
			reloadCtx, stop := context.WithTimeout(ctx, 5*time.Second)
			err = s.reloadEnrollment(reloadCtx, reporting, request.ConfigPath, version, historyLoaded)
			stop()
			if err != nil {
				ack.Error = err.Error()
			}
			if err := writeEnrollmentReload(filepath.Join(s.stateDir, "enrollment-reload-result.json"), ack); err != nil {
				s.logger.Warn("write enrollment reload result", "error", err)
			}
		}
	}()
	return cancel, done
}
