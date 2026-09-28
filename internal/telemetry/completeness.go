package telemetry

import (
	"errors"
	"time"
)

const EventTelemetryCheckpoint EventKind = "telemetry_checkpoint"

// Completeness measures events selected by the capture policy, before the
// bounded async queue. It does not claim to measure uninstrumented activity.
type Completeness struct {
	AttemptedEvents        uint64      `json:"attempted_events"`
	LastSequence           uint64      `json:"last_sequence"`
	QueueDroppedEvents     uint64      `json:"queue_dropped_events"`
	SinkFailedEvents       uint64      `json:"sink_failed_events"`
	OmittedEvents          uint64      `json:"omitted_events"`
	SessionStartedObserved bool        `json:"session_started_observed"`
	SessionEndedObserved   bool        `json:"session_ended_observed"`
	CaptureKinds           []EventKind `json:"capture_kinds"`
	IncludeRedactedText    bool        `json:"include_redacted_text"`
}

type completenessState struct {
	event     Event
	stats     Completeness
	processed uint64
	revision  uint64
	dirty     bool
}

func (s *AsyncSink) setCapturePolicy(kinds []EventKind, includeText bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.captureKinds = append([]EventKind{}, kinds...)
	s.includeText = includeText
}

// Caller holds s.mu. Never retain event text or discovery secrets here.
func (s *AsyncSink) completenessLocked(event Event) *completenessState {
	if event.SessionID == "" || event.Kind == EventTelemetryCheckpoint {
		return nil
	}
	key := eventKey(event)
	state := s.sessions[key]
	if state == nil {
		state = &completenessState{event: Event{SchemaVersion: 2, SourceID: event.SourceID, ActorID: event.ActorID, SessionID: event.SessionID, ProjectID: event.ProjectID, Provider: event.Provider, Kind: EventTelemetryCheckpoint}}
		state.stats.CaptureKinds = append([]EventKind{}, s.captureKinds...)
		state.stats.IncludeRedactedText = s.includeText
		s.sessions[key] = state
	}
	state.stats.SessionStartedObserved = state.stats.SessionStartedObserved || event.Kind == EventSessionStarted
	state.stats.SessionEndedObserved = state.stats.SessionEndedObserved || event.Kind == EventSessionEnded
	state.revision++
	state.dirty = true
	return state
}

// Filtered lifecycle boundaries still describe the capture's scope, without
// consuming a selected-event sequence number or going through the lossy queue.
func (s *AsyncSink) observeLifecycle(event Event) {
	if event.Kind != EventSessionStarted && event.Kind != EventSessionEnded {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.completenessLocked(event)
	s.wakeCheckpoint()
}

func (s *AsyncSink) wakeCheckpoint() {
	select {
	case s.checkpointWake <- struct{}{}:
	default:
	}
}

func (s *AsyncSink) flushCheckpoints(endedOnly bool) error {
	type pending struct {
		key      sessionIdentity
		event    Event
		revision uint64
	}
	var snapshots []pending
	s.mu.Lock()
	for key, state := range s.sessions {
		if !state.dirty || (endedOnly && !state.stats.SessionEndedObserved) || state.processed+state.stats.QueueDroppedEvents != state.stats.AttemptedEvents {
			continue
		}
		stats := state.stats
		event := state.event
		event.Timestamp = time.Now().UTC()
		event.Completeness = &stats
		snapshots = append(snapshots, pending{key, event, state.revision})
	}
	s.mu.Unlock()
	var failures []error
	for _, snapshot := range snapshots {
		if s.sink == nil {
			continue
		}
		if err := s.sink.Record(snapshot.event); err != nil {
			failures = append(failures, err)
			if s.onError != nil {
				s.onError(err)
			}
			continue
		}
		s.mu.Lock()
		if state := s.sessions[snapshot.key]; state != nil && state.revision == snapshot.revision {
			state.dirty = false
			if state.stats.SessionEndedObserved {
				delete(s.sessions, snapshot.key)
			}
		}
		s.mu.Unlock()
	}
	return errors.Join(failures...)
}
