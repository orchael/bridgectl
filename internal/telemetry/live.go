package telemetry

import (
	"context"
	"fmt"
	"sync"
	"unicode/utf8"
)

// maxInteractionBufferSize is deliberately larger than the semantic framer's
// bound so long valid full-capture records are retained. Exceptionally large
// unterminated records become explicit omission events rather than unbounded
// process memory.
const maxInteractionBufferSize = 1 << 20

// LiveCollector combines framing, analysis, and bounded asynchronous delivery.
type LiveCollector struct {
	analyzer    *Analyzer
	async       *AsyncSink
	framer      *Framer
	onError     func(error)
	sourceID    string
	identity    LiveIdentity
	mu          sync.Mutex
	pending     map[sessionIdentity]*interactionBuffer
	contextSink *sessionContextSink
}

type interactionBuffer struct {
	direction Direction
	stream    StreamType
	data      []byte
}

type LiveIdentity struct {
	SourceID    string
	ActorID     string
	SourceLabel string
	ContextKey  []byte
}

func NewLiveCollector(sink Sink, queueSize int, includeRedactedText bool, onError func(error), kinds ...EventKind) *LiveCollector {
	return NewLiveCollectorForSource(sink, queueSize, includeRedactedText, "", onError, kinds...)
}

func NewLiveCollectorForSource(sink Sink, queueSize int, includeRedactedText bool, sourceID string, onError func(error), kinds ...EventKind) *LiveCollector {
	return NewLiveCollectorWithIdentity(sink, queueSize, includeRedactedText, LiveIdentity{SourceID: sourceID}, onError, kinds...)
}

func NewLiveCollectorWithIdentity(sink Sink, queueSize int, includeRedactedText bool, identity LiveIdentity, onError func(error), kinds ...EventKind) *LiveCollector {
	contextSink := &sessionContextSink{sink: sink, discover: DiscoverSessionContext}
	async := NewAsyncSink(contextSink, queueSize, onError)
	filtered := NewFilteredSink(async, kinds...)
	return &LiveCollector{
		analyzer:    NewAnalyzer(filtered, nil, WithIncludeRedactedText(includeRedactedText)),
		async:       async,
		framer:      NewFramer(defaultFrameBufferSize),
		onError:     onError,
		sourceID:    identity.SourceID,
		identity:    identity,
		pending:     make(map[sessionIdentity]*interactionBuffer),
		contextSink: contextSink,
	}
}

func (c *LiveCollector) sourceSession(session Session) Session {
	if c.sourceID != "" {
		session.SourceID = c.sourceID
	}
	if c.identity.ActorID != "" {
		session.ActorID = c.identity.ActorID
	}
	return session
}

func frameKey(session Session) string { return session.SourceID + "\x00" + session.SessionID }

func (c *LiveCollector) SessionStarted(session Session) {
	session = c.sourceSession(session)
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pending, sessionKey(session))
	c.framer.Reset(frameKey(session))
	c.analyzer.ObserveSessionStart(session)
	c.analyzer.record(Event{
		Timestamp: c.analyzer.now().UTC(), SourceID: session.SourceID, ActorID: session.ActorID,
		SessionID: session.SessionID, ProjectID: session.ProjectID, Provider: session.Provider,
		Kind: EventSessionContext, contextDiscovery: &sessionContextDiscovery{
			repoPath: session.RepoPath, actorID: session.ActorID,
			sourceLabel: c.identity.SourceLabel, key: c.identity.ContextKey,
		},
	})
}

func (c *LiveCollector) ObserveOutputChunk(session Session, data []byte) {
	c.ObserveProviderChunk(session, StreamOutput, data)
}

func (c *LiveCollector) ObserveProviderChunk(session Session, stream StreamType, data []byte) {
	session = c.sourceSession(session)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.observeInteraction(session, DirectionAgent, stream, data)
}

func (c *LiveCollector) ObserveInputChunk(session Session, data []byte) {
	session = c.sourceSession(session)
	c.mu.Lock()
	defer c.mu.Unlock()
	key := sessionKey(session)
	if pending := c.pending[key]; pending != nil && pending.direction == DirectionAgent {
		c.flushInteraction(session, pending)
		delete(c.pending, key)
		if frame := c.framer.FlushOutput(frameKey(session)); frame != "" {
			c.analyzer.ObserveOutput(session, []byte(frame))
		}
	}
	c.observeInteraction(session, DirectionHuman, StreamInput, data)
}

func (c *LiveCollector) SessionEnded(session Session) {
	session = c.sourceSession(session)
	c.mu.Lock()
	defer c.mu.Unlock()
	key := sessionKey(session)
	if pending := c.pending[key]; pending != nil {
		c.flushInteraction(session, pending)
		delete(c.pending, key)
	}
	if frame := c.framer.FlushOutput(frameKey(session)); frame != "" {
		c.analyzer.ObserveOutput(session, []byte(frame))
	}
	if frame := c.framer.FlushInput(frameKey(session)); frame != "" {
		c.analyzer.ObserveInput(session, []byte(frame))
	}
	c.analyzer.ObserveSessionEnd(session)
}

func (c *LiveCollector) observeInteraction(session Session, direction Direction, stream StreamType, data []byte) {
	if len(data) == 0 {
		return
	}
	key := sessionKey(session)
	pending := c.pending[key]
	if pending != nil && (pending.direction != direction || pending.stream != stream) {
		c.flushInteraction(session, pending)
		pending = nil
	}
	if pending == nil {
		pending = &interactionBuffer{direction: direction, stream: stream}
		c.pending[key] = pending
	}
	combined := append(append([]byte(nil), pending.data...), data...)
	if len(pending.data) > 0 && utf8.Valid(pending.data) && !validOrIncompleteUTF8(combined) {
		c.flushInteraction(session, pending)
		pending.data = nil
		combined = append(combined[:0], data...)
	}
	pending.data = combined
	for {
		boundary := nextInteractionBoundary(pending.data)
		if boundary == 0 {
			break
		}
		if boundary > maxInteractionBufferSize {
			c.analyzer.ObserveOmittedInteraction(session, direction, interactionKind(direction), stream, pending.data[:boundary], OmittedBufferLimit)
		} else {
			c.emitInteraction(session, direction, stream, pending.data[:boundary])
		}
		pending.data = append([]byte(nil), pending.data[boundary:]...)
	}
	if len(pending.data) > maxInteractionBufferSize {
		c.analyzer.ObserveOmittedInteraction(session, direction, interactionKind(direction), stream, pending.data, OmittedBufferLimit)
		pending.data = nil
		delete(c.pending, key)
		return
	}
}

// A malformed record is omitted as one privacy unit, but later valid records
// remain recoverable. Newlines inside OSC/DCS payloads are not record boundaries.
func nextInteractionBoundary(data []byte) int {
	buffer := string(data)
	for i := 0; i < len(buffer); i++ {
		if buffer[i] == '\x1b' {
			end, complete := ansiSequenceEnd(buffer, i)
			if !complete {
				return 0
			}
			i = end - 1
			continue
		}
		if isRecordBoundary(buffer[i]) {
			return i + 1
		}
	}
	return 0
}

func (c *LiveCollector) flushInteraction(session Session, pending *interactionBuffer) {
	if len(pending.data) == 0 {
		return
	}
	c.emitInteraction(session, pending.direction, pending.stream, pending.data)
	pending.data = nil
}

func (c *LiveCollector) emitInteraction(session Session, direction Direction, stream StreamType, data []byte) {
	if direction == DirectionAgent {
		c.analyzer.ObserveProviderInteraction(session, stream, data)
		if stream == StreamOutput && utf8.Valid(data) {
			for _, frame := range c.framer.FeedOutput(frameKey(session), data) {
				c.analyzer.ObserveOutput(session, []byte(frame))
			}
		}
		return
	}
	c.analyzer.ObserveUserInteraction(session, data)
	if utf8.Valid(data) {
		for _, frame := range c.framer.FeedInput(frameKey(session), data) {
			c.analyzer.ObserveInput(session, []byte(frame))
		}
	}
}

func interactionKind(direction Direction) EventKind {
	if direction == DirectionAgent {
		return EventProviderOutput
	}
	return EventUserInput
}

func isRecordBoundary(value byte) bool {
	switch value {
	case '\n', '\r':
		return true
	default:
		return false
	}
}

func validOrIncompleteUTF8(data []byte) bool {
	if utf8.Valid(data) {
		return true
	}
	for suffix := 1; suffix <= 3 && suffix <= len(data); suffix++ {
		prefix, tail := data[:len(data)-suffix], data[len(data)-suffix:]
		if utf8.Valid(prefix) && !utf8.FullRune(tail) {
			return true
		}
	}
	return false
}

func (c *LiveCollector) Feedback() Feedback { return c.analyzer.Feedback() }
func (c *LiveCollector) Dropped() uint64    { return c.async.Dropped() }
func (c *LiveCollector) Failures() uint64   { return c.async.Failures() }
func (c *LiveCollector) Close(ctx context.Context) error {
	err := c.async.Close(ctx)
	if dropped := c.async.Dropped(); dropped > 0 && c.onError != nil {
		c.onError(fmt.Errorf("telemetry queue dropped %d events", dropped))
	}
	return err
}
