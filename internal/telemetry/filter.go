package telemetry

func validEventKind(kind EventKind) bool {
	switch kind {
	case EventSessionStarted, EventProviderOutput, EventUserInput, EventQuestion, EventAnswer, EventSessionEnded:
		return true
	default:
		return false
	}
}

// FilteredSink forwards only explicitly allowed event kinds. An empty
// allow-list preserves all events for backward compatibility.
type FilteredSink struct {
	sink    Sink
	allowed map[EventKind]struct{}
}

func NewFilteredSink(sink Sink, kinds ...EventKind) *FilteredSink {
	allowed := make(map[EventKind]struct{}, len(kinds))
	for _, kind := range kinds {
		allowed[kind] = struct{}{}
	}
	return &FilteredSink{sink: sink, allowed: allowed}
}

func (s *FilteredSink) Record(event Event) error {
	if len(s.allowed) > 0 {
		if _, ok := s.allowed[event.Kind]; !ok {
			return nil
		}
	}
	if s.sink == nil {
		return nil
	}
	return s.sink.Record(event)
}
