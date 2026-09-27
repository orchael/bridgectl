package bridgecontrol

import (
	"errors"
	"sync"
	"time"

	"github.com/orchael/bridgectl/internal/bridge"
)

// TerminalRequest is an ephemeral control request. ClientID must be scoped by
// the authenticated gateway to the user and browser tab, never a local CLI ID.
type TerminalRequest struct {
	ID             string    `json:"request_id"`
	OrganizationID string    `json:"organization_id"`
	InstallationID string    `json:"installation_id"`
	SessionID      string    `json:"session_id"`
	ClientID       string    `json:"client_id"`
	Action         string    `json:"action"`
	KeepWriter     bool      `json:"keep_writer,omitempty"`
	AfterSequence  uint64    `json:"after_sequence"`
	Data           []byte    `json:"data,omitempty"`
	Cols           uint32    `json:"cols,omitempty"`
	Rows           uint32    `json:"rows,omitempty"`
	ExpiresAt      time.Time `json:"expires_at"`
}
type TerminalResult struct {
	ID     string                `json:"request_id"`
	Window bridge.TerminalWindow `json:"window"`
	Code   string                `json:"code,omitempty"`
}
type terminalLease struct {
	timer           *time.Timer
	stop            chan struct{}
	session, client string
	expires         time.Time
}
type terminalManager struct {
	mu         sync.Mutex
	leases     map[string]*terminalLease
	supervisor func() *bridge.Supervisor
	ttl        time.Duration
}

func (m *terminalManager) release(key string) {
	l := m.leases[key]
	if l == nil {
		return
	}
	l.timer.Stop()
	close(l.stop)
	if s := m.supervisor(); s != nil {
		if writer, _ := s.Detach(l.session, l.client); writer {
			s.NotifyWriterReleased(l.session, l.client)
		}
	}
	delete(m.leases, key)
}
func (m *terminalManager) close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key := range m.leases {
		m.release(key)
	}
}
func (m *terminalManager) handle(r TerminalRequest) TerminalResult {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := TerminalResult{ID: r.ID}
	fail := func(code string) TerminalResult { result.Code = code; return result }
	if r.ID == "" || len(r.ID) > 128 || r.SessionID == "" || len(r.SessionID) > 256 || len(r.ClientID) < 1 || len(r.ClientID) > 128 || len(r.Data) > 16384 || time.Now().After(r.ExpiresAt) || r.ExpiresAt.After(time.Now().Add(30*time.Second)) {
		return fail("invalid_request")
	}
	if m.supervisor == nil {
		return fail("unsupported")
	}
	s := m.supervisor()
	if s == nil {
		return fail("unsupported")
	}
	key := r.SessionID + "\x00" + r.ClientID
	var err error
	switch r.Action {
	case "watch":
		if l := m.leases[key]; l != nil {
			if r.KeepWriter {
				l.expires = time.Now().Add(m.ttl)
				l.timer.Reset(m.ttl)
			} else {
				m.release(key)
			}
		}
	case "attach":
		if m.leases == nil {
			m.leases = make(map[string]*terminalLease)
		}
		if m.leases[key] == nil {
			if len(m.leases) >= 64 {
				return fail("busy")
			}
			var state *bridge.AttachState
			state, err = s.Attach(r.SessionID, r.ClientID, r.AfterSequence, bridge.AttachRoleWriter)
			if err == nil {
				if state.Role != bridge.AttachRoleWriter || state.ExitRecorded {
					_, _ = s.Detach(r.SessionID, r.ClientID)
					return fail("session_ended")
				}
				l := &terminalLease{stop: make(chan struct{}), session: r.SessionID, client: r.ClientID, expires: time.Now().Add(m.ttl)}
				l.timer = time.AfterFunc(m.ttl, func() {
					m.mu.Lock()
					defer m.mu.Unlock()
					if m.leases[key] == l {
						if remaining := time.Until(l.expires); remaining > 0 {
							l.timer.Reset(remaining)
							return
						}
						m.release(key)
					}
				})
				m.leases[key] = l
				go func() {
					for {
						select {
						case _, ok := <-state.Live:
							if !ok {
								return
							}
						case <-l.stop:
							return
						}
					}
				}()
				s.NotifyWriterClaimed(r.SessionID, r.ClientID)
			}
		}
	case "detach":
		m.release(key)
	case "input":
		if m.leases[key] == nil {
			return fail("not_attached")
		}
		_, err = s.WriteInput(r.SessionID, r.ClientID, r.Data)
	case "resize":
		if m.leases[key] == nil {
			return fail("not_attached")
		}
		if r.Cols < 2 || r.Cols > 500 || r.Rows < 1 || r.Rows > 300 {
			return fail("invalid_request")
		}
		err = s.Resize(r.SessionID, r.ClientID, r.Cols, r.Rows)
	default:
		return fail("invalid_request")
	}
	if err != nil {
		if errors.Is(err, bridge.ErrWriterConflict) {
			return fail("writer_conflict")
		}
		return fail("unavailable")
	}
	result.Window, err = s.TerminalWindow(r.SessionID, r.ClientID, r.AfterSequence)
	if err != nil {
		return fail("unavailable")
	}
	if !result.Window.Writer && m.leases[key] != nil {
		m.release(key)
	}
	return result
}
