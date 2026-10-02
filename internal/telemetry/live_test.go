package telemetry

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLiveCollectorFramesProviderFixtures(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			fixture := readFixture(t, provider+"-permission.txt")
			sink := &memorySink{}
			collector := NewLiveCollector(sink, 16, true, nil,
				EventSessionStarted, EventQuestion, EventAnswer, EventSessionEnded,
			)
			session := Session{SessionID: provider + "-session", Provider: provider}
			collector.SessionStarted(session)
			for _, chunk := range splitFixture(fixture) {
				collector.ObserveOutputChunk(session, []byte(chunk))
			}
			collector.ObserveInputChunk(session, []byte("y"))
			collector.ObserveInputChunk(session, []byte("es\r"))
			collector.SessionEnded(session)
			if err := collector.Close(context.Background()); err != nil {
				t.Fatal(err)
			}

			events := capturedEvents(sink.snapshot())
			if len(events) != 4 {
				t.Fatalf("events=%+v, want lifecycle/question/answer/lifecycle", events)
			}
			if events[1].Kind != EventQuestion || events[1].Class != ClassPermission {
				t.Fatalf("question event=%+v", events[1])
			}
			if events[2].Decision != DecisionAccepted {
				t.Fatalf("answer event=%+v", events[2])
			}
		})
	}
}

func TestLiveCollectorDeduplicatesRedrawAndCanOmitText(t *testing.T) {
	sink := &memorySink{}
	collector := NewLiveCollector(sink, 16, false, nil)
	session := Session{SessionID: "s1", Provider: "codex"}
	collector.SessionStarted(session)
	collector.ObserveOutputChunk(session, []byte("Do you want me to run tests?\r"))
	collector.ObserveOutputChunk(session, []byte("Do you want me to run tests?\r"))
	collector.ObserveInputChunk(session, []byte("no\n"))
	collector.SessionEnded(session)
	if err := collector.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	events := capturedEvents(sink.snapshot())
	questions := 0
	for _, event := range events {
		if event.Kind == EventQuestion {
			questions++
		}
		if event.Text != "" {
			t.Fatalf("event retained text with include_text disabled: %+v", event)
		}
	}
	if questions != 1 {
		t.Fatalf("questions=%d, want 1", questions)
	}
	feedback := collector.Feedback()
	if len(feedback.Questions) != 1 || feedback.Questions[0].Example != "" {
		t.Fatalf("feedback=%+v", feedback)
	}
}

func TestLiveCollectorFiltersEventKindsBeforeQueueing(t *testing.T) {
	sink := &memorySink{}
	collector := NewLiveCollector(sink, 16, false, nil, EventQuestion, EventAnswer)
	session := Session{SessionID: "filtered", Provider: "codex"}
	collector.SessionStarted(session)
	collector.ObserveOutputChunk(session, []byte("Proceed?"))
	collector.ObserveInputChunk(session, []byte("yes\n"))
	collector.SessionEnded(session)
	if err := collector.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	events := capturedEvents(sink.snapshot())
	if len(events) != 2 || events[0].Kind != EventQuestion || events[1].Kind != EventAnswer || events[0].Sequence != 1 || events[1].Sequence != 2 {
		t.Fatalf("filtered events=%+v", events)
	}
}

// TEL-114: full capture retains ordered, redacted traffic from both sides while
// continuing to emit the derived question/answer events used by reports.
func TestLiveCollectorCapturesFullBidirectionalInteraction(t *testing.T) {
	sink := &memorySink{}
	collector := NewLiveCollectorForSource(sink, 32, true, "bridge-live", nil,
		EventProviderOutput, EventUserInput, EventQuestion, EventAnswer,
	)
	session := Session{SessionID: "full-capture", ProjectID: "project", Provider: "codex"}
	collector.SessionStarted(session)
	collector.ObserveProviderChunk(session, StreamOutput, []byte("status token=top-secret\n"))
	collector.ObserveProviderChunk(session, StreamThinking, []byte("considering alternatives"))
	collector.ObserveProviderChunk(session, StreamOutput, []byte("\x1b[33mProceed?\x1b[0m"))
	collector.ObserveProviderChunk(session, StreamOutput, []byte{0xff, 0xfe})
	collector.ObserveInputChunk(session, []byte("yes\n"))
	collector.SessionEnded(session)
	if err := collector.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	events := capturedEvents(sink.snapshot())
	if len(events) != 7 {
		t.Fatalf("events=%+v, want four provider chunks, user input, question, and answer", events)
	}
	var providerEvents, userEvents int
	var sawThinking, sawQuestion, sawAnswer, sawInvalid bool
	for index, event := range events {
		if event.SourceID != "bridge-live" {
			t.Fatalf("event[%d].SourceID=%q, want bridge-live", index, event.SourceID)
		}
		if event.SchemaVersion != 2 {
			t.Fatalf("event[%d].SchemaVersion=%d, want 2", index, event.SchemaVersion)
		}
		if index > 0 && event.Sequence <= events[index-1].Sequence {
			t.Fatalf("event sequences are not increasing: %+v", events)
		}
		if strings.Contains(event.Text, "top-secret") || strings.Contains(event.Text, "\x1b") {
			t.Fatalf("event[%d] retained unsafe content: %+v", index, event)
		}
		switch event.Kind {
		case EventProviderOutput:
			providerEvents++
			sawThinking = sawThinking || event.Stream == StreamThinking
			if event.OmittedReason == OmittedInvalidUTF8 {
				sawInvalid = event.Text == "" && event.ByteCount == 2
			}
		case EventUserInput:
			userEvents++
			if event.Direction != DirectionHuman || event.Text != "yes\n" {
				t.Fatalf("user input event=%+v", event)
			}
		case EventQuestion:
			sawQuestion = true
		case EventAnswer:
			sawAnswer = event.Decision == DecisionAccepted
		}
	}
	if providerEvents != 4 || userEvents != 1 || !sawThinking || !sawQuestion || !sawAnswer || !sawInvalid {
		t.Fatalf("incomplete full capture: events=%+v", events)
	}
}

func TestLiveCollectorReassemblesSplitUTF8AndSecrets(t *testing.T) {
	sink := &memorySink{}
	collector := NewLiveCollector(sink, 32, true, nil, EventProviderOutput, EventUserInput, EventQuestion, EventAnswer)
	session := Session{SessionID: "split", Provider: "codex"}
	collector.ObserveOutputChunk(session, []byte{'P', 'r', 'o', 'c', 'e', 'e', 'd', '?', ' ', 0xe2})
	collector.ObserveOutputChunk(session, []byte{0x82, 0xac})
	collector.ObserveInputChunk(session, []byte{'y', 0xc3})
	collector.ObserveInputChunk(session, []byte{0xa9, 's', '\n'})
	collector.ObserveOutputChunk(session, []byte("token=super-"))
	collector.ObserveOutputChunk(session, []byte("secret done\n"))
	collector.ObserveOutputChunk(session, []byte("api_key "))
	collector.ObserveOutputChunk(session, []byte("=second-secret done\n"))
	collector.SessionEnded(session)
	if err := collector.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	events := capturedEvents(sink.snapshot())
	joined := ""
	var sawEuro, sawInput, sawRedaction bool
	for _, event := range events {
		joined += event.Text
		sawEuro = sawEuro || strings.Contains(event.Text, "€")
		sawInput = sawInput || event.Kind == EventUserInput && event.Text == "yés\n"
		sawRedaction = sawRedaction || event.Redactions > 0
	}
	if strings.Contains(joined, "super-secret") || strings.Contains(joined, "second-secret") || !sawEuro || !sawInput || !sawRedaction {
		t.Fatalf("split chunks were not safely reconstructed: %+v", events)
	}
}

func TestLiveCollectorKeepsMultilineTerminalControlsPrivate(t *testing.T) {
	sink := &memorySink{}
	collector := NewLiveCollector(sink, 8, true, nil, EventProviderOutput, EventQuestion)
	session := Session{SessionID: "terminal-string"}
	collector.ObserveOutputChunk(session, []byte("before\x1b]0;private?\n"))
	collector.ObserveOutputChunk(session, []byte("payload\x07after\n"))
	collector.SessionEnded(session)
	if err := collector.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	events := capturedEvents(sink.snapshot())
	if len(events) != 1 || events[0].Kind != EventProviderOutput || events[0].Text != "beforeafter\n" {
		t.Fatalf("terminal payload leaked or split a question: %+v", events)
	}
}

func TestLiveCollectorPreservesRecordsAfterInvalidUTF8(t *testing.T) {
	sink := &memorySink{}
	collector := NewLiveCollector(sink, 8, true, nil, EventProviderOutput)
	session := Session{SessionID: "invalid-then-valid"}
	collector.ObserveOutputChunk(session, append([]byte{0xff, '\n'}, []byte("recoverable output\n")...))
	collector.SessionEnded(session)
	if err := collector.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	events := capturedEvents(sink.snapshot())
	if len(events) != 2 || events[0].OmittedReason != OmittedInvalidUTF8 || events[0].ByteCount != 2 || events[1].Text != "recoverable output\n" {
		t.Fatalf("invalid record discarded a recoverable valid record: %+v", events)
	}
}

// TestLiveCollectorRetainsLongValidInteraction proves TEL-114: the semantic
// question framer's 16 KiB bound must not discard a valid full-capture record.
func TestLiveCollectorRetainsLongValidInteraction(t *testing.T) {
	sink := &memorySink{}
	collector := NewLiveCollector(sink, 8, true, nil, EventProviderOutput)
	session := Session{SessionID: "long-interaction", Provider: "codex"}
	content := strings.Repeat("x", defaultFrameBufferSize+1)
	collector.ObserveOutputChunk(session, []byte(content))
	collector.SessionEnded(session)
	if err := collector.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	events := capturedEvents(sink.snapshot())
	if len(events) != 1 || events[0].Text != content || events[0].OmittedReason != "" {
		t.Fatalf("long interaction was not retained: %+v", events)
	}
}

func TestLiveCollectorBoundsOversizedUnterminatedInteraction(t *testing.T) {
	sink := &memorySink{}
	collector := NewLiveCollector(sink, 8, true, nil, EventProviderOutput)
	session := Session{SessionID: "oversized-interaction", Provider: "codex"}
	content := strings.Repeat("x", maxInteractionBufferSize+1)
	collector.ObserveOutputChunk(session, []byte(content))
	collector.SessionEnded(session)
	if err := collector.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	events := capturedEvents(sink.snapshot())
	if len(events) != 1 || events[0].Text != "" || events[0].OmittedReason != OmittedBufferLimit || events[0].ByteCount != len(content) || events[0].ContentHash == "" {
		t.Fatalf("oversized interaction was not bounded with explicit metadata: %+v", events)
	}
}

// TestLiveCollectorHandlesANSISequenceSplitAcrossManyChunks is a correctness
// check: an ANSI escape sequence arriving one byte per call must still be
// recognized once it completes, not treated as a record boundary partway
// through.
func TestLiveCollectorHandlesANSISequenceSplitAcrossManyChunks(t *testing.T) {
	sink := &memorySink{}
	collector := NewLiveCollector(sink, 8, true, nil, EventProviderOutput)
	session := Session{SessionID: "split-ansi-sequence", Provider: "codex"}

	const paramBytes = 50
	collector.ObserveOutputChunk(session, []byte("\x1b[")) // begin a CSI sequence
	for i := 0; i < paramBytes; i++ {
		collector.ObserveOutputChunk(session, []byte("0")) // valid CSI parameter byte
	}
	collector.ObserveOutputChunk(session, []byte("m\n")) // terminate, then close the record

	collector.SessionEnded(session)
	if err := collector.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	events := capturedEvents(sink.snapshot())
	wantBytes := len("\x1b[") + paramBytes + len("m\n")
	if len(events) != 1 || events[0].ByteCount != wantBytes || events[0].OmittedReason != "" {
		t.Fatalf("events=%+v, want one retained interaction of %d bytes", events, wantBytes)
	}
}

// TestLiveCollectorDesyncSplitsOversizedValidTextFromInvalidTail is a
// correctness regression test for a Copilot review finding on this PR: an
// earlier version of the fix bounded the UTF-8 desync check (see
// advanceUTF8State) to a fixed buffer size to avoid re-validating the whole
// accumulated buffer on every call, which silently disabled the desync
// split once the pending buffer exceeded that size — merging a large valid
// record with a later invalid tail into a single invalid_utf8 omission
// instead of preserving the valid text as its own record. advanceUTF8State
// tracks state incrementally instead of size-gating, so this must still
// split correctly well past any such threshold.
func TestLiveCollectorDesyncSplitsOversizedValidTextFromInvalidTail(t *testing.T) {
	sink := &memorySink{}
	collector := NewLiveCollector(sink, 8, true, nil, EventProviderOutput)
	session := Session{SessionID: "oversized-then-invalid", Provider: "codex"}

	validText := strings.Repeat("x", 20000) // well past the old 16 KiB bound
	collector.ObserveOutputChunk(session, []byte(validText))
	collector.ObserveOutputChunk(session, []byte{0xff, '\n'})
	collector.SessionEnded(session)
	if err := collector.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	events := capturedEvents(sink.snapshot())
	if len(events) != 2 || events[0].Text != validText || events[0].OmittedReason != "" ||
		events[1].OmittedReason != OmittedInvalidUTF8 || events[1].ByteCount != 2 {
		t.Fatalf("desync check did not split oversized valid text from the invalid tail: got %d events, want 2 (valid text=%d bytes, then invalid_utf8=2 bytes)", len(events), len(validText))
	}
}

// TestLiveCollectorSingleLongIncompleteANSISequenceStaysLinear is a
// regression test for a Copilot review finding on this PR: the resume
// cursor fixed in nextInteractionBoundary only prevented rescanning
// *resolved* prefix data — a single ANSI escape sequence that itself grows
// across many chunks (e.g. a long CSI parameter list delivered a few bytes
// at a time, with no newline) still made ansiSequenceEnd re-scan the whole
// open sequence from its start on every call, leaving the same O(n^2)
// failure mode up to the 1MB flush. ansiSequenceEndFrom's own resume cursor
// (escStart/escScanned) must keep this linear too.
func TestLiveCollectorSingleLongIncompleteANSISequenceStaysLinear(t *testing.T) {
	sink := &memorySink{}
	collector := NewLiveCollector(sink, 8, true, nil, EventProviderOutput)
	session := Session{SessionID: "long-incomplete-ansi-sequence", Provider: "codex"}

	const paramBytes = 50000
	start := time.Now()
	collector.ObserveOutputChunk(session, []byte("\x1b[")) // begin an unterminated CSI sequence
	for i := 0; i < paramBytes; i++ {
		// '0' (0x30) is a valid CSI parameter byte, so the sequence never
		// terminates — every call must re-examine it as still-incomplete.
		collector.ObserveOutputChunk(session, []byte("0"))
	}
	collector.ObserveOutputChunk(session, []byte("m\n")) // terminate the sequence, then close the record
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("feeding %d one-byte chunks of a single open escape sequence took %v, want well under 2s (quadratic regression?)", paramBytes, elapsed)
	}

	collector.SessionEnded(session)
	if err := collector.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	events := capturedEvents(sink.snapshot())
	wantBytes := len("\x1b[") + paramBytes + len("m\n")
	if len(events) != 1 || events[0].ByteCount != wantBytes || events[0].OmittedReason != "" {
		t.Fatalf("events=%+v, want one retained interaction of %d bytes", events, wantBytes)
	}
}

// TestLiveCollectorManyChunksWithoutBoundaryStaysLinear reproduces the
// CPU-usage bug behind a real bridgectl deployment reporting "server start"
// pinning a CPU core under an active, chatty TUI session. A full-screen
// redraw TUI (common for interactive coding agents) repositions the cursor
// with short, complete ANSI sequences rather than emitting \r/\n, so
// pending output can grow for a long stretch with no record boundary. Three
// independent O(n^2) costs compounded here, all scaling with the total
// bytes buffered before a boundary appears rather than the bytes in each
// chunk:
//  1. nextInteractionBoundary rescanned data from byte 0 on every call.
//  2. observeInteraction's UTF-8 desync check (utf8.Valid) re-validated the
//     entire accumulated buffer on every call.
//  3. trimFrameBuffer removed leading runes one at a time, re-encoding the
//     entire remaining (still large) rune slice on every iteration just to
//     recheck its byte length.
//
// Confirmed via a live pprof CPU profile during an attached codex session:
// appendChunk -> ObserveProviderChunk -> observeInteraction ->
// nextInteractionBoundary/ansiSequenceEnd/utf8.Valid accounted for 100% of
// sampled CPU time. Before these fixes, this test's chunk count took
// multiple seconds (trimFrameBuffer alone) to well over a minute (all
// three); after, it completes in milliseconds.
func TestLiveCollectorManyChunksWithoutBoundaryStaysLinear(t *testing.T) {
	sink := &memorySink{}
	collector := NewLiveCollector(sink, 8, true, nil, EventProviderOutput)
	session := Session{SessionID: "slow-redraw-stream", Provider: "codex"}

	const chunkCount = 5000
	const chunk = "\x1b[2K\x1b[1;32mx" // short, complete ANSI sequences + text, no boundary
	start := time.Now()
	for i := 0; i < chunkCount; i++ {
		collector.ObserveOutputChunk(session, []byte(chunk))
	}
	collector.ObserveOutputChunk(session, []byte("\n"))
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("feeding %d chunks with no boundary took %v, want well under 2s (quadratic regression?)", chunkCount, elapsed)
	}

	collector.SessionEnded(session)
	if err := collector.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	events := capturedEvents(sink.snapshot())
	wantBytes := chunkCount*len(chunk) + len("\n")
	if len(events) != 1 || events[0].ByteCount != wantBytes || events[0].OmittedReason != "" {
		t.Fatalf("events=%+v, want one retained interaction of %d bytes", events, wantBytes)
	}
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(data), `\x1b`, "\x1b")
}

func splitFixture(fixture string) []string {
	mid := len(fixture) / 2
	return []string{fixture[:mid], fixture[mid:]}
}

func TestLiveCollectorSinkFailureDoesNotDelayObservation(t *testing.T) {
	sink := sinkFunc(func(Event) error {
		time.Sleep(200 * time.Millisecond)
		return nil
	})
	collector := NewLiveCollector(sink, 1, true, nil)
	session := Session{SessionID: "s", Provider: "codex"}
	start := time.Now()
	collector.SessionStarted(session)
	collector.ObserveOutputChunk(session, []byte("Proceed?"))
	collector.ObserveInputChunk(session, []byte("yes\n"))
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("observation blocked for %v", elapsed)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = collector.Close(ctx)
}

func TestLiveCollectorContextDiscoveryDoesNotBlockSessionIO(t *testing.T) {
	sink := &memorySink{}
	collector := NewLiveCollector(sink, 8, true, nil, EventSessionStarted, EventSessionContext, EventProviderOutput, EventSessionEnded)
	discoveryStarted := make(chan struct{})
	releaseDiscovery := make(chan struct{})
	collector.contextSink.discover = func(string, string, string, []byte) SessionContext {
		close(discoveryStarted)
		<-releaseDiscovery
		return SessionContext{OS: "linux", Arch: "amd64"}
	}
	session := Session{SessionID: "slow-context", RepoPath: "/private/repo"}
	startDone := make(chan struct{})
	go func() {
		collector.SessionStarted(session)
		close(startDone)
	}()
	<-discoveryStarted
	select {
	case <-startDone:
	case <-time.After(100 * time.Millisecond):
		close(releaseDiscovery)
		<-startDone
		_ = collector.Close(context.Background())
		t.Fatal("context filesystem discovery blocked session startup")
	}
	collector.ObserveOutputChunk(session, []byte("output\n"))
	close(releaseDiscovery)
	collector.SessionEnded(session)
	if err := collector.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	events := capturedEvents(sink.snapshot())
	if len(events) != 4 || events[0].Kind != EventSessionStarted || events[1].Kind != EventSessionContext || events[2].Kind != EventProviderOutput || events[3].Kind != EventSessionEnded || events[1].Context == nil {
		t.Fatalf("discovery changed lifecycle/event order: %+v", events)
	}
	for _, event := range events {
		if event.contextDiscovery != nil {
			t.Fatal("private filesystem discovery request reached persistence sink")
		}
	}
}

func TestFramerBoundsIncompleteData(t *testing.T) {
	framer := NewFramer(32)
	frames := framer.FeedOutput("s", []byte(strings.Repeat("x", 128)))
	if len(frames) != 0 {
		t.Fatalf("frames=%v, want none", frames)
	}
	if got := framer.BufferedOutput("s"); len(got) > 32 {
		t.Fatalf("buffer length=%d, want <=32", len(got))
	}
}

func TestFramerKeepsTerminalStringControlsWhole(t *testing.T) {
	for _, control := range []string{"\x1b]0;private?\x07", "\x1b]0;private?\x1b\\", "\x1bPprivate?\x1b\\"} {
		framer := NewFramer(1024)
		input := "before" + control + "after\n"
		cut := strings.IndexByte(input, '?') + 1
		if frames := framer.FeedOutput("session", []byte(input[:cut])); len(frames) != 0 {
			t.Fatalf("incomplete control produced frames: %q", frames)
		}
		frames := framer.FeedOutput("session", []byte(input[cut:]))
		if len(frames) != 1 || normalize(frames[0]) != "beforeafter" {
			t.Fatalf("control payload split logical output: %q", frames)
		}
	}
}

func TestFramerDoesNotTreatANSIPrivateMarkerAsQuestion(t *testing.T) {
	framer := NewFramer(128)
	if frames := framer.FeedOutput("s", []byte("\x1b[?25")); len(frames) != 0 {
		t.Fatalf("partial ANSI frames=%q, want none", frames)
	}
	frames := framer.FeedOutput("s", []byte("hready\nProceed?"))
	if len(frames) != 2 || !strings.Contains(frames[0], "ready") || frames[1] != "Proceed?" {
		t.Fatalf("frames=%q", frames)
	}
}

// Capture assertions concern sequenced data. Checkpoint content and loss
// reporting are covered separately in completeness_test.go.
func capturedEvents(events []Event) []Event {
	var captured []Event
	for _, event := range events {
		if event.Kind != EventTelemetryCheckpoint {
			captured = append(captured, event)
		}
	}
	return captured
}
