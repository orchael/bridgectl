package bridge

// TerminalWindow is a bounded, transient view of retained PTY output. Data is
// encoded as base64 by JSON so split UTF-8 and escape sequences stay intact.
type TerminalWindow struct {
	Data         []byte `json:"data"`
	NextSequence uint64 `json:"next_sequence"`
	Gap          bool   `json:"gap"`
	Ended        bool   `json:"ended"`
	Writer       bool   `json:"writer"`
	Cols         uint32 `json:"cols"`
	Rows         uint32 `json:"rows"`
}

// TerminalWindow reads without attaching, claiming a writer, or resizing a PTY.
func (s *Supervisor) TerminalWindow(sessionID, clientID string, after uint64) (TerminalWindow, error) {
	s.mu.RLock()
	ms := s.sessions[sessionID]
	s.mu.RUnlock()
	if ms == nil {
		return TerminalWindow{}, ErrSessionNotFound
	}
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if ms.streamJSON {
		return TerminalWindow{}, ErrSessionRecoveryUnavailable
	}
	ms.buf.mu.RLock()
	defer ms.buf.mu.RUnlock()
	var oldest, last uint64
	if len(ms.buf.chunks) > 0 {
		oldest = ms.buf.chunks[0].Seq
		last = ms.buf.chunks[len(ms.buf.chunks)-1].Seq
	}
	w := TerminalWindow{NextSequence: after, Cols: ms.info.Cols, Rows: ms.info.Rows, Writer: ms.info.ActiveWriterClientID == clientID && clientID != ""}
	w.Gap = (after > 0 && oldest > after+1) || after > last
	if w.Gap {
		after = 0
		w.NextSequence = 0
	}
	for _, chunk := range ms.buf.chunks {
		if chunk.Seq <= after {
			continue
		}
		if len(w.Data)+len(chunk.Payload) > 65536 {
			break
		}
		w.Data = append(w.Data, chunk.Payload...)
		w.NextSequence = chunk.Seq
	}
	w.Ended = ms.liveClosed && w.NextSequence == last
	return w, nil
}
