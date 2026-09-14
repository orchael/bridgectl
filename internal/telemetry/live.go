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
}

func NewLiveCollector(sink Sink, queueSize int, includeRedactedText bool, onError func(error), kinds ...EventKind) *LiveCollector {
	async := NewAsyncSink(sink, queueSize, onError)
	filtered := NewFilteredSink(async, kinds...)
	return &LiveCollector{
		analyzer: NewAnalyzer(filtered, nil, WithIncludeRedactedText(includeRedactedText)),
		async:    async,
		framer:   NewFramer(defaultFrameBufferSize),
		onError:  onError,
	}
}

func (c *LiveCollector) SessionStarted(session Session) {
	c.framer.Reset(session.SessionID)
	c.analyzer.ObserveSessionStart(session)
}

func (c *LiveCollector) ObserveOutputChunk(session Session, data []byte) {
	c.ObserveProviderChunk(session, StreamOutput, data)
}

func (c *LiveCollector) ObserveProviderChunk(session Session, stream StreamType, data []byte) {
	c.analyzer.ObserveProviderInteraction(session, stream, data)
	if stream != StreamOutput || !utf8.Valid(data) {
		return
	}
	for _, frame := range c.framer.FeedOutput(session.SessionID, data) {
		c.analyzer.ObserveOutput(session, []byte(frame))
	}
}

func (c *LiveCollector) ObserveInputChunk(session Session, data []byte) {
	c.analyzer.ObserveUserInteraction(session, data)
	if !utf8.Valid(data) {
		return
	}
	for _, frame := range c.framer.FeedInput(session.SessionID, data) {
		c.analyzer.ObserveInput(session, []byte(frame))
	}
}

func (c *LiveCollector) SessionEnded(session Session) {
	if frame := c.framer.FlushOutput(session.SessionID); frame != "" {
		c.analyzer.ObserveOutput(session, []byte(frame))
	}
	if frame := c.framer.FlushInput(session.SessionID); frame != "" {
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
