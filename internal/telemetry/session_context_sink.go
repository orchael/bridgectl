package telemetry

import "context"

// Discovery requests remain private in-memory work and are removed before an
// event reaches a persistence/network sink. A single bounded AsyncSink worker
// owns filesystem discovery, preserving start/context/output order.
type sessionContextDiscovery struct {
	repoPath    string
	actorID     string
	sourceLabel string
	key         []byte
}

type sessionContextSink struct {
	sink     Sink
	discover func(string, string, string, []byte) SessionContext
}

func (s *sessionContextSink) Record(event Event) error {
	if request := event.contextDiscovery; request != nil {
		event.contextDiscovery = nil
		if event.Kind == EventSessionContext {
			metadata := s.discover(request.repoPath, request.actorID, request.sourceLabel, request.key)
			event.Context = &metadata
		}
	}
	if s.sink == nil {
		return nil
	}
	return s.sink.Record(event)
}

func (s *sessionContextSink) Close(ctx context.Context) error {
	if closer, ok := s.sink.(interface{ Close(context.Context) error }); ok {
		return closer.Close(ctx)
	}
	return nil
}
