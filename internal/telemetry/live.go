package telemetry

import (
	"context"
	"fmt"
	"unicode/utf8"
)

// LiveCollector combines framing, analysis, and bounded asynchronous delivery.
type LiveCollector struct {
	analyzer *Analyzer
	async    *AsyncSink
	framer   *Framer
	onError  func(error)
	sourceID string
}

func NewLiveCollector(sink Sink, queueSize int, includeRedactedText bool, onError func(error), kinds ...EventKind) *LiveCollector {
	return NewLiveCollectorForSource(sink, queueSize, includeRedactedText, "", onError, kinds...)
}

func NewLiveCollectorForSource(sink Sink, queueSize int, includeRedactedText bool, sourceID string, onError func(error), kinds ...EventKind) *LiveCollector {
	async := NewAsyncSink(sink, queueSize, onError)
	filtered := NewFilteredSink(async, kinds...)
	return &LiveCollector{
		analyzer: NewAnalyzer(filtered, nil, WithIncludeRedactedText(includeRedactedText)),
		async:    async,
		framer:   NewFramer(defaultFrameBufferSize),
		onError:  onError,
		sourceID: sourceID,
	}
}

func (c *LiveCollector) sourceSession(session Session) Session {
	if c.sourceID != "" {
		session.SourceID = c.sourceID
	}
	return session
}

func frameKey(session Session) string { return session.SourceID + "\x00" + session.SessionID }

func (c *LiveCollector) SessionStarted(session Session) {
	session = c.sourceSession(session)
	c.framer.Reset(frameKey(session))
	c.analyzer.ObserveSessionStart(session)
}

func (c *LiveCollector) ObserveOutputChunk(session Session, data []byte) {
	c.ObserveProviderChunk(session, StreamOutput, data)
}

func (c *LiveCollector) ObserveProviderChunk(session Session, stream StreamType, data []byte) {
	session = c.sourceSession(session)
	c.analyzer.ObserveProviderInteraction(session, stream, data)
	if stream != StreamOutput || !utf8.Valid(data) {
		return
	}
	for _, frame := range c.framer.FeedOutput(frameKey(session), data) {
		c.analyzer.ObserveOutput(session, []byte(frame))
	}
}

func (c *LiveCollector) ObserveInputChunk(session Session, data []byte) {
	session = c.sourceSession(session)
	c.analyzer.ObserveUserInteraction(session, data)
	if !utf8.Valid(data) {
		return
	}
	for _, frame := range c.framer.FeedInput(frameKey(session), data) {
		c.analyzer.ObserveInput(session, []byte(frame))
	}
}

func (c *LiveCollector) SessionEnded(session Session) {
	session = c.sourceSession(session)
	if frame := c.framer.FlushOutput(frameKey(session)); frame != "" {
		c.analyzer.ObserveOutput(session, []byte(frame))
	}
	if frame := c.framer.FlushInput(frameKey(session)); frame != "" {
		c.analyzer.ObserveInput(session, []byte(frame))
	}
	c.analyzer.ObserveSessionEnd(session)
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
