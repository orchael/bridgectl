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

			events := sink.snapshot()
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

	events := sink.snapshot()
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
	events := sink.snapshot()
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

	events := sink.snapshot()
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
	events := sink.snapshot()
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
