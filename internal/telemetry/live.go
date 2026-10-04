package telemetry

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
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
	// scanned is the resume cursor for nextInteractionBoundary: data[:scanned]
	// is confirmed to contain no boundary and no open ANSI escape sequence,
	// so the next call only needs to examine data[scanned:]. Without this, a
	// TUI that emits ANSI sequences split across PTY reads with no newline
	// for a while made every incoming chunk rescan (and the caller re-copy)
	// the entire buffered-so-far data from byte 0 — O(n^2) in the total
	// bytes buffered before a boundary appears.
	scanned int
	// After an unsafe frame, retry only after the buffer doubles. This bounds
	// total redaction scanning for short redraws or incomplete credentials.
	frameRetryAt int
	// escStart is the start index (within data) of a currently-open,
	// not-yet-complete ANSI escape sequence, or -1 when none is open.
	// escScanned is how far the terminator search within that sequence has
	// already progressed (an absolute index into data). Without resuming
	// from here, an escape sequence delivered a few bytes at a time (e.g. a
	// long CSI parameter list split across PTY reads) made ansiSequenceEnd
	// re-scan the sequence from its start on every call — still O(n^2) even
	// after the outer scanned cursor above stopped the rest of the buffer
	// from being rescanned.
	escStart   int
	escScanned int
	// utf8 tracks just enough of data's trailing bytes to answer "does data
	// currently end on a clean UTF-8 boundary" without re-validating the
	// whole buffer on every call — see advanceUTF8State.
	utf8 utf8State
}

// utf8State is the result of validOrIncompleteUTF8-style analysis of a
// buffer's end, carried forward across calls instead of recomputed from the
// full buffer each time.
type utf8State struct {
	// broken is true once the buffer contains a non-trailing encoding
	// error. utf8.Valid never becomes true again for such a buffer no
	// matter what gets appended (a bad byte earlier in the stream isn't
	// fixed by later bytes), so once broken this is an O(1) no-op forever,
	// until the buffer is reset or truncated at a boundary.
	broken bool
	// tail holds the (at most 3) trailing bytes that are a valid prefix of
	// an as-yet-incomplete rune. Empty/nil when data ends on a clean
	// boundary (equivalent to utf8.Valid(data) == true).
	tail []byte
}

// advanceUTF8State folds newData onto prev (the state of the buffer newData
// is being appended to) without looking at any of the buffer's earlier
// bytes: when prev is clean or incomplete, only prev.tail (<=3 bytes) plus
// newData can possibly be relevant to whether the result ends cleanly,
// since everything before that tail was already confirmed complete.
// Re-running utf8.Valid/validOrIncompleteUTF8 on the whole accumulated
// buffer on every incoming chunk reintroduced the same O(n^2) blowup the
// scan-resume cursor above fixed for nextInteractionBoundary.
func advanceUTF8State(prev utf8State, newData []byte) utf8State {
	if prev.broken {
		return prev
	}
	window := append(append([]byte(nil), prev.tail...), newData...)
	if utf8.Valid(window) {
		return utf8State{}
	}
	for suffix := 1; suffix <= 3 && suffix <= len(window); suffix++ {
		prefix, tail := window[:len(window)-suffix], window[len(window)-suffix:]
		if utf8.Valid(prefix) && !utf8.FullRune(tail) {
			return utf8State{tail: append([]byte(nil), tail...)}
		}
	}
	return utf8State{broken: true}
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
	async.setCapturePolicy(kinds, includeRedactedText)
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
		pending = &interactionBuffer{direction: direction, stream: stream, escStart: -1}
		c.pending[key] = pending
	}
	// Appending directly onto pending.data (rather than copying it into a
	// fresh buffer first) is safe here: a reset below only ever happens
	// before anything reads pending.data's old content past what
	// advanceUTF8State/resetNeeded already captured.
	resetNeeded := len(pending.data) > 0 && !pending.utf8.broken && len(pending.utf8.tail) == 0 && !validOrIncompleteUTF8(data)
	if resetNeeded {
		c.flushInteraction(session, pending)
		pending.data = nil
		pending.scanned = 0
		pending.frameRetryAt = 0
		pending.escStart = -1
		pending.utf8 = utf8State{}
	}
	pending.data = append(pending.data, data...)
	pending.utf8 = advanceUTF8State(pending.utf8, data)
	for {
		boundary := nextInteractionBoundary(pending)
		if boundary == 0 {
			break
		}
		if boundary > maxInteractionBufferSize {
			c.analyzer.ObserveOmittedInteraction(session, direction, interactionKind(direction), stream, pending.data[:boundary], OmittedBufferLimit)
		} else {
			c.emitInteraction(session, direction, stream, pending.data[:boundary])
		}
		remainder := pending.data[boundary:]
		pending.data = append([]byte(nil), remainder...)
		pending.scanned = 0
		pending.frameRetryAt = 0
		pending.escStart = -1
		// The remainder was never checked for UTF-8 completeness on its
		// own (only as a suffix of the now-discarded, boundary-terminated
		// prefix), so re-derive its state fresh. This is a one-time cost
		// per boundary found, proportional to the remainder at that
		// moment — not repeated per subsequent chunk, so it doesn't
		// reintroduce the O(n^2) this function exists to avoid.
		pending.utf8 = advanceUTF8State(utf8State{}, pending.data)
	}
	if len(pending.data) > maxInteractionBufferSize {
		c.analyzer.ObserveOmittedInteraction(session, direction, interactionKind(direction), stream, pending.data, OmittedBufferLimit)
		pending.data = nil
		pending.scanned = 0
		pending.frameRetryAt = 0
		pending.escStart = -1
		pending.utf8 = utf8State{}
		delete(c.pending, key)
		return
	}
}

// nextInteractionBoundary scans pending.data for the next interaction
// boundary (an unescaped newline/carriage return), resuming from
// pending.scanned/escStart/escScanned — the position and in-progress escape
// sequence state confirmed by a previous call — instead of rescanning from
// byte 0. A malformed record is omitted as one privacy unit, but later valid
// records remain recoverable. Newlines inside OSC/DCS payloads are not
// record boundaries.
//
// Without resuming, a TUI that emits ANSI sequences split across PTY reads
// with no newline for a while made every incoming chunk rescan (and the
// caller re-copy) the entire buffered-so-far data from byte 0 — O(n^2) in
// the total bytes buffered before a boundary appears. Resuming the scan
// cursor alone isn't enough for a single escape sequence that itself grows
// across many chunks (e.g. a very long CSI parameter list delivered a few
// bytes at a time): ansiSequenceEnd's own terminator search also needs to
// resume from escScanned rather than re-scanning that sequence from its
// start on every call.
//
// Updates pending.scanned/escStart/escScanned in place and returns the
// boundary position, or 0 when none is found yet.
func nextInteractionBoundary(pending *interactionBuffer) int {
	data := pending.data
	if pending.scanned < 0 || pending.scanned > len(data) {
		pending.scanned = 0
		pending.frameRetryAt = 0
	}
	i := pending.scanned
	if pending.escStart >= 0 {
		end, complete := ansiSequenceEndFrom(data, pending.escStart, pending.escScanned)
		if !complete {
			pending.escScanned = end
			return 0
		}
		frameComplete := pending.direction == DirectionAgent && pending.stream == StreamOutput && bytes.Equal(data[pending.escStart:end], []byte("\x1b[?2026l"))
		i = end
		pending.escStart = -1
		if frameComplete {
			if boundary := pending.frameBoundary(end); boundary > 0 {
				return boundary
			}
		}
	}
	for i < len(data) {
		if data[i] == '\x1b' {
			end, complete := ansiSequenceEndFrom(data, i, i+2)
			if !complete {
				pending.escStart = i
				pending.escScanned = end
				pending.scanned = i
				return 0
			}
			// Full-screen providers redraw without line endings. A completed
			// synchronized update is a record boundary, including when split
			// across PTY reads, so frames are archived while the session runs.
			if pending.direction == DirectionAgent && pending.stream == StreamOutput && bytes.Equal(data[i:end], []byte("\x1b[?2026l")) {
				if boundary := pending.frameBoundary(end); boundary > 0 {
					return boundary
				}
			}
			i = end
			continue
		}
		if isRecordBoundary(data[i]) {
			return i + 1
		}
		i++
	}
	pending.scanned = i
	return 0
}

// Geometric retry spacing makes unsuccessful safety checks linear in total
// buffered bytes. Ordinary record boundaries still flush immediately; a safe
// frame resets this state when its emitted prefix is removed.
func (pending *interactionBuffer) frameBoundary(end int) int {
	if end < pending.frameRetryAt {
		return 0
	}
	boundary := safeFrameBoundary(pending.data[:end])
	if boundary == 0 {
		pending.frameRetryAt = end * 2
	}
	return boundary
}

// Frame markers are zero-width controls, not redaction boundaries. Retain
// trailing words and any credential expression crossing the candidate cut so
// the next redraw can complete it before DefaultRedactor sees the record.
// The existing interaction buffer cap also bounds this carryover; overflowing
// records produce an explicit omission instead of publishing an unsafe prefix.
var frameWordsRE = regexp.MustCompile(`\S+`)
var framePendingSecretRE = regexp.MustCompile(`(?i)\b` + secretKeyPattern + `"?\s*(?:[:=]\s*(?:"|bearer\s*)?)?$`)
var framePrivateStartRE = regexp.MustCompile(`-----BEGIN [A-Z ]*`)

func safeFrameBoundary(raw []byte) int {
	if !utf8.Valid(raw) {
		return len(raw)
	} // Preserve malformed records as omission units.
	escapes := ansiRE.FindAllIndex(raw, -1)
	text := make([]byte, 0, len(raw))
	offsets := make([]int, 0, len(raw))
	nextEscape := 0
	for i := 0; i < len(raw); {
		if nextEscape < len(escapes) && i == escapes[nextEscape][0] {
			i = escapes[nextEscape][1]
			nextEscape++
			continue
		}
		text = append(text, raw[i])
		offsets = append(offsets, i)
		i++
	}
	words := frameWordsRE.FindAllIndex(text, -1)
	if len(words) < 3 {
		return 0
	}
	cut := words[len(words)-2][0]
	if pending := framePendingSecretRE.FindIndex(text); pending != nil && pending[0] < cut {
		cut = pending[0]
	}
	for _, start := range framePrivateStartRE.FindAllIndex(text, -1) {
		complete := privateKeyRE.FindIndex(text[start[0]:])
		if (complete == nil || complete[0] != 0) && start[0] < cut {
			cut = start[0]
		}
	}
	// Moving the cut left can intersect another overlapping credential match.
	for {
		previous := cut
		for _, pattern := range []*regexp.Regexp{privateKeyRE, bearerRE, credentialValueRE, quotedSecretRE, secretRE} {
			for _, match := range pattern.FindAllIndex(text, -1) {
				if match[0] < cut && match[1] > cut {
					cut = match[0]
				}
			}
		}
		if cut == previous {
			break
		}
	}
	if cut == 0 {
		return 0
	}
	return offsets[cut]
}

// ansiSequenceEndFrom reports where the ANSI escape sequence starting at
// data[start] ends, resuming the terminator search from scanFrom (an
// absolute index into data) instead of always restarting at start+2. The
// sequence "kind" is determined solely by data[start+1], which never
// changes once the sequence begins, so it's always cheap to re-derive.
// Mirrors ansiSequenceEnd's logic (framing.go) but operates on []byte with
// a resumable scan instead of re-scanning a fresh string from the
// sequence's start on every call — needed because a single escape sequence
// can itself grow across many chunks (e.g. a long CSI parameter list
// delivered a few bytes at a time).
func ansiSequenceEndFrom(data []byte, start, scanFrom int) (int, bool) {
	if start+1 >= len(data) {
		return len(data), false
	}
	introducer := data[start+1]
	if scanFrom < start+2 {
		scanFrom = start + 2
	}
	if introducer == ']' || introducer == 'P' || introducer == 'X' || introducer == '^' || introducer == '_' {
		for i := scanFrom; i < len(data); i++ {
			if introducer == ']' && data[i] == '\x07' {
				return i + 1, true
			}
			if data[i] == '\x1b' {
				if i+1 >= len(data) {
					// Trailing ESC with no lookahead byte yet: resume
					// exactly here next call so a terminating '\\'
					// arriving as the very next chunk's first byte is
					// still recognized, instead of being skipped over.
					return i, false
				}
				if data[i+1] == '\\' {
					return i + 2, true
				}
			}
		}
		return len(data), false
	}
	if introducer != '[' {
		return start + 2, true
	}
	for i := scanFrom; i < len(data); i++ {
		// ECMA-48 control sequence terminators occupy 0x40 through 0x7e.
		if data[i] >= 0x40 && data[i] <= 0x7e {
			return i + 1, true
		}
	}
	return len(data), false
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
