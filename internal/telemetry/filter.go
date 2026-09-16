package telemetry

import "sync"

func validEventKind(kind EventKind) bool {
	switch kind {
	case EventSessionStarted, EventSessionContext, EventProviderOutput, EventUserInput, EventQuestion, EventAnswer, EventSessionEnded:
		return true
	default:
		return false
	}
}

// FilteredSink forwards only explicitly allowed event kinds. An empty
// allow-list preserves all events for backward compatibility.
type FilteredSink struct {
	sink     Sink
	allowed  map[EventKind]struct{}
	mu       sync.Mutex
	sequence map[sessionIdentity]uint64
}

func NewFilteredSink(sink Sink, kinds ...EventKind) *FilteredSink {
	allowed := make(map[EventKind]struct{}, len(kinds))
	for _, kind := range kinds {
		allowed[kind] = struct{}{}
	}
	return &FilteredSink{sink: sink, allowed: allowed, sequence: make(map[sessionIdentity]uint64)}
}

func (s *FilteredSink) Record(event Event) error {
	if len(s.allowed) > 0 {
		s.mu.Lock()
		defer s.mu.Unlock()
		key := eventKey(event)
		if event.Kind == EventSessionStarted {
			delete(s.sequence, key)
		}
		if event.Kind == EventSessionEnded {
			defer delete(s.sequence, key)
		}
		if _, ok := s.allowed[event.Kind]; !ok {
			return nil
		}
		s.sequence[key]++
		event.Sequence = s.sequence[key]
	}
	if s.sink == nil {
		return nil
	}
	return s.sink.Record(event)
}
