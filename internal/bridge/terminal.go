package bridge

import (
	"context"
	"time"
)

// StoredReplayPage exposes a bounded page to authenticated attach transports.
// A nil page means this store has no paged replay capability.
func (s *Supervisor) StoredReplayPage(session string, after uint64) ([]OutputChunk, error) {
	if store, ok := s.store.(ChunkPageStore); ok {
		return store.LoadChunkPage(session, after, 65536)
	}
	return nil, nil
}

// TerminalWindow is a bounded, transient view of retained PTY output. Data is
// encoded as base64 by JSON so split UTF-8 and escape sequences stay intact.
type TerminalWindow struct {
	More          bool   `json:"more"`
	Data          []byte `json:"data"`
	FirstSequence uint64 `json:"first_sequence,omitempty"`
	NextSequence  uint64 `json:"next_sequence"`
	Gap           bool   `json:"gap"`
	Ended         bool   `json:"ended"`
	Writer        bool   `json:"writer"`
	Cols          uint32 `json:"cols"`
	Rows          uint32 `json:"rows"`
}

// TerminalWindow reads without attaching, claiming a writer, or resizing a PTY.
func (s *Supervisor) TerminalWindow(sessionID, clientID string, after uint64) (TerminalWindow, error) {
	s.mu.RLock()
	ms := s.sessions[sessionID]
	s.mu.RUnlock()
	if ms == nil {
		s.histMu.RLock()
		info, ok := s.history[sessionID]
		s.histMu.RUnlock()
		if !ok {
			return TerminalWindow{}, ErrSessionNotFound
		}
		ms = &managedSession{info: info, buf: NewByteBuffer(1), liveClosed: true}
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
	last = max(last, ms.info.LastSeq, ms.buf.nextSeq-1)
	w := TerminalWindow{NextSequence: after, Cols: ms.info.Cols, Rows: ms.info.Rows, Writer: ms.info.ActiveWriterClientID == clientID && clientID != ""}
	chunks := ms.buf.chunks
	// Prefer durable output when the requested cursor predates the memory ring.
	// Seek by sequence on disk, returning at most one wire-sized page per call.
	if after < last && (oldest == 0 || oldest > after+1) {
		if store, ok := s.store.(ChunkPageStore); ok {
			page, err := store.LoadChunkPage(sessionID, after, 65536)
			if err != nil {
				return TerminalWindow{}, err
			}
			if len(page) > 0 {
				chunks = page
				oldest = page[0].Seq
			}
		}
	}
	if len(chunks) == 0 && after < last {
		return TerminalWindow{}, ErrSessionRecoveryUnavailable
	}
	w.Gap = oldest > after+1 || after > last
	if w.Gap {
		// A retained suffix is not a terminal checkpoint. It can start inside
		// UTF-8 or an escape sequence and depends on lost cursor/mode state.
		// Report the gap without feeding corrupt bytes to the viewer. It may
		// rebuild from sequence zero using the durable journal, or use history
		// when the beginning of that journal is no longer available.
		w.FirstSequence = oldest
		w.NextSequence = last
		w.Ended = ms.liveClosed
		return w, nil
	}
	first := true
	for _, chunk := range chunks {
		if chunk.Seq <= after {
			continue
		}
		if !first && chunk.Seq != w.NextSequence+1 {
			break
		}
		if chunk.Cols > 0 && chunk.Rows > 0 {
			if !first && (w.Cols != chunk.Cols || w.Rows != chunk.Rows) {
				break
			}
			w.Cols, w.Rows = chunk.Cols, chunk.Rows
		}
		if len(w.Data)+len(chunk.Payload) > 65536 {
			if first {
				return TerminalWindow{}, ErrSessionRecoveryUnavailable
			}
			break
		}
		if first {
			w.FirstSequence = chunk.Seq
			first = false
		}
		w.Data = append(w.Data, chunk.Payload...)
		w.NextSequence = chunk.Seq
	}
	w.More = w.NextSequence < last
	w.Ended = ms.liveClosed && w.NextSequence == last
	return w, nil
}

// WaitTerminalWindow wakes on new output. A one-second heartbeat also carries
// end/resize/writer changes, without polling through the network gateways.
func (s *Supervisor) WaitTerminalWindow(ctx context.Context, session, client string, after uint64) (TerminalWindow, error) {
	s.mu.RLock()
	ms := s.sessions[session]
	s.mu.RUnlock()
	if ms == nil {
		return s.TerminalWindow(session, client, after)
	}
	changed := ms.buf.changes()
	w, err := s.TerminalWindow(session, client, after)
	if err != nil || w.NextSequence != after || len(w.Data) > 0 || w.Gap || w.Ended {
		return w, err
	}
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-changed:
	case <-timer.C:
	case <-ctx.Done():
		return TerminalWindow{}, ctx.Err()
	}
	return s.TerminalWindow(session, client, after)
}
