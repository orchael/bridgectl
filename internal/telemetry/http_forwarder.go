package telemetry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const httpForwarderMaxBackoff = 30 * time.Second

// HTTPForwardingSink delivers the existing normalized JSONL spool to Bridge's
// HTTPS collector. The credential is held only in memory and is never logged.
type HTTPForwardingSink struct {
	spool                *SegmentSpool
	endpoint, credential string
	flushInterval        time.Duration
	retryInterval        time.Duration
	client               *http.Client
	stop                 chan struct{}
	done                 chan struct{}
	ctx                  context.Context
	cancel               context.CancelFunc
}

func (s *HTTPForwardingSink) Record(event Event) error { return s.spool.Record(event) }

// NewHTTPForwardingSink starts delivery on flushInterval, retrying sooner
// (with exponential backoff starting at retryInterval, capped at 30s) after
// a failed delivery, matching GRPCForwardingSink's retry behavior. A single
// timer schedules every attempt so a pending backoff is never raced by an
// independent flush tick, which would otherwise retry an unavailable
// collector more often than the backoff intends.
func NewHTTPForwardingSink(spool *SegmentSpool, endpoint, credential string, flushInterval, retryInterval time.Duration, onError func(error)) *HTTPForwardingSink {
	if flushInterval <= 0 {
		flushInterval = 10 * time.Second
	}
	if retryInterval <= 0 {
		retryInterval = time.Second
	}
	retryInterval = min(retryInterval, httpForwarderMaxBackoff)
	ctx, cancel := context.WithCancel(context.Background())
	s := &HTTPForwardingSink{spool: spool, endpoint: endpoint, credential: credential, flushInterval: flushInterval, retryInterval: retryInterval, client: &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}, stop: make(chan struct{}), done: make(chan struct{}), ctx: ctx, cancel: cancel}
	go func() {
		defer close(s.done)
		timer := time.NewTimer(0)
		defer timer.Stop()
		backoff := retryInterval
		for {
			select {
			case <-s.stop:
				return
			case <-timer.C:
				if err := s.upload(s.ctx); err != nil {
					if onError != nil {
						onError(err)
					}
					resetTimer(timer, backoff)
					backoff = min(backoff*2, httpForwarderMaxBackoff)
				} else {
					backoff = retryInterval
					resetTimer(timer, flushInterval)
				}
			}
		}
	}()
	return s
}
func (s *HTTPForwardingSink) upload(ctx context.Context) error {
	if _, err := s.spool.Seal(); err != nil {
		return err
	}
	segs, err := s.spool.Pending()
	if err != nil {
		return err
	}
	for _, seg := range segs {
		data, err := s.spool.Read(seg.ID)
		if err != nil {
			return err
		}
		payload, err := bridgeEnvelope(seg.ID, data)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+s.credential)
		resp, err := s.client.Do(req)
		if err != nil {
			return err
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			_ = body
			return fmt.Errorf("telemetry upload returned HTTP %d", resp.StatusCode)
		}
		if err := s.spool.Remove(seg.ID); err != nil {
			return err
		}
	}
	return nil
}
func (s *HTTPForwardingSink) Close(ctx context.Context) error {
	close(s.stop)
	select {
	case <-s.done:
		return s.upload(ctx)
	case <-ctx.Done():
		// The worker goroutine is still running an in-flight upload on s.ctx;
		// cancel it so a stalled collector cannot hold it open past the
		// caller's shutdown deadline.
		s.cancel()
		return ctx.Err()
	}
}
func bridgeEnvelope(id string, jsonl []byte) ([]byte, error) {
	events := make([]map[string]any, 0)
	for _, line := range bytes.Split(bytes.TrimSpace(jsonl), []byte("\n")) {
		var e Event
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, err
		}
		payload := map[string]any{"schema_version": e.SchemaVersion, "sequence": e.Sequence}
		if e.Class != "" {
			payload["class"] = e.Class
		}
		if e.Decision != "" {
			payload["decision"] = e.Decision
		}
		if e.ByteCount > 0 {
			payload["byte_count"] = e.ByteCount
		}
		events = append(events, map[string]any{"event_id": fmt.Sprintf("%s-%d", id, len(events)+1), "schema_version": e.SchemaVersion, "occurred_at": e.Timestamp.UTC().Format(time.RFC3339Nano), "session_id": e.SessionID, "provider": func() string {
			if e.Provider != "" {
				return e.Provider
			}
			return "unknown"
		}(), "event_type": string(e.Kind), "payload": payload})
	}
	created := time.Unix(0, 0).UTC()
	if len(id) >= len("20060102T150405.000000000Z") {
		if parsed, parseErr := time.Parse("20060102T150405.000000000Z", id[:len("20060102T150405.000000000Z")]); parseErr == nil {
			created = parsed
		}
	}
	segment := map[string]any{"created_at": created.Format(time.RFC3339Nano), "collector": map[string]string{"name": "bridgectl", "version": "dev"}, "events": events}
	raw, err := json.Marshal(segment)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	return json.Marshal(map[string]any{"schema_version": 1, "segment_id": id, "segment_checksum": "sha256:" + hex.EncodeToString(sum[:]), "segment": json.RawMessage(raw)})
}
