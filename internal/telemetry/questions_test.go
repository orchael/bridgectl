package telemetry

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

type memorySink struct {
	mu     sync.Mutex
	events []Event
}

func (s *memorySink) Record(e Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
	return nil
}

func (s *memorySink) snapshot() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Event(nil), s.events...)
}

type errorSink struct{}

func (errorSink) Record(Event) error { return errors.New("sink unavailable") }

func TestAcceptedPermissionQuestion(t *testing.T) {
	sink := &memorySink{}
	a := NewAnalyzer(sink, nil)
	now := time.Date(2026, 9, 13, 17, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }
	session := Session{SessionID: "s1", ProjectID: "p1", Provider: "claude"}

	a.ObserveOutput(session, []byte("Do you want me to run this command?"))
	now = now.Add(2 * time.Second)
	a.ObserveInput(session, []byte("yes\n"))

	if len(sink.events) != 2 {
		t.Fatalf("events=%d, want 2", len(sink.events))
	}
	if sink.events[0].Class != ClassPermission {
		t.Fatalf("class=%q, want %q", sink.events[0].Class, ClassPermission)
	}
	if sink.events[1].Decision != DecisionAccepted {
		t.Fatalf("decision=%q, want %q", sink.events[1].Decision, DecisionAccepted)
	}
	if sink.events[1].LatencyMS != 2000 {
		t.Fatalf("latency=%d, want 2000", sink.events[1].LatencyMS)
	}

	feedback := a.Feedback()
	if len(feedback.Questions) != 1 || feedback.Questions[0].Accepted != 1 {
		t.Fatalf("unexpected feedback: %+v", feedback)
	}
}

func TestAnalyzerUsesCompositeSessionIdentity(t *testing.T) {
	sink := &memorySink{}
	analyzer := NewAnalyzer(sink, nil)
	sourceA := Session{SourceID: "bridge-a", SessionID: "shared", Provider: "codex"}
	sourceB := Session{SourceID: "bridge-b", SessionID: "shared", Provider: "codex"}

	analyzer.ObserveOutput(sourceA, []byte("Proceed with this command?"))
	analyzer.ObserveOutput(sourceB, []byte("Which environment should I use?"))
	analyzer.ObserveInput(sourceA, []byte("yes"))
	analyzer.ObserveInput(sourceB, []byte("staging"))

	events := sink.snapshot()
	if len(events) != 4 {
		t.Fatalf("events=%d, want two independently correlated question/answer pairs", len(events))
	}
	answers := map[string]Event{}
	for _, event := range events {
		if event.SourceID == "" {
			t.Fatal("event omitted source_id")
		}
		if event.Kind == EventAnswer {
			answers[event.SourceID] = event
		}
	}
	if answers["bridge-a"].Decision != DecisionAccepted || answers["bridge-a"].Sequence != 2 {
		t.Fatalf("bridge-a answer=%+v", answers["bridge-a"])
	}
	if answers["bridge-b"].Decision != DecisionUnknown || answers["bridge-b"].Sequence != 2 {
		t.Fatalf("bridge-b answer=%+v", answers["bridge-b"])
	}
}

func TestRejectAndChangedAreNotAutoApprovalCandidates(t *testing.T) {
	a := NewAnalyzer(nil, nil)
	session := Session{SessionID: "s1", Provider: "codex"}

	a.ObserveOutput(session, []byte("Should I deploy this to production?"))
	a.ObserveInput(session, []byte("no"))

	a.ObserveOutput(session, []byte("Which environment should I use?"))
	a.ObserveInput(session, []byte("Use staging first, then show me the result."))

	feedback := a.Feedback()
	var rejected, changed int
	for _, q := range feedback.Questions {
		rejected += q.Rejected
		changed += q.Changed
	}
	if rejected != 1 || changed != 1 {
		t.Fatalf("rejected=%d changed=%d, want 1/1", rejected, changed)
	}
}

func TestRedactsSecrets(t *testing.T) {
	sink := &memorySink{}
	a := NewAnalyzer(sink, nil)
	session := Session{SessionID: "s1", Provider: "claude"}

	a.ObserveOutput(session, []byte("Allow this command with token=super-secret?"))
	if len(sink.events) != 1 {
		t.Fatalf("events=%d, want 1", len(sink.events))
	}
	if got := sink.events[0].Text; strings.Contains(got, "super-secret") {
		t.Fatalf("secret was not redacted: %q", got)
	}
}

func TestDefaultRedactorCoversFullCaptureCredentialFamilies(t *testing.T) {
	awsAccessKeyFixture := "AKIA" + "ABCDEFGHIJKLMNOP"
	tests := []struct {
		name   string
		secret string
		input  string
	}{
		{name: "bearer", secret: "bearer-secret-value", input: "Authorization: Bearer bearer-secret-value"},
		{name: "compound AWS secret", secret: "short-secret", input: "AWS_SECRET_ACCESS_KEY=short-secret"},
		{name: "compound client secret", secret: "client-value", input: "CLIENT_SECRET=client-value"},
		{name: "openai", secret: "sk-abcdefghijklmnop", input: "use sk-abcdefghijklmnop now"},
		{name: "github", secret: "ghp_abcdefghijklmnop", input: "use ghp_abcdefghijklmnop now"},
		{name: "aws", secret: awsAccessKeyFixture, input: "use " + awsAccessKeyFixture + " now"},
		{name: "private key", secret: "private-material", input: "-----BEGIN PRIVATE KEY-----\nprivate-material\n-----END PRIVATE KEY-----"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := DefaultRedactor(test.input); strings.Contains(got, test.secret) || !strings.Contains(got, "[REDACTED:") {
				t.Fatalf("DefaultRedactor(%q)=%q", test.input, got)
			}
		})
	}
}

func TestQuestionClasses(t *testing.T) {
	tests := []struct {
		name string
		text string
		want QuestionClass
	}{
		{name: "permission", text: "Allow this command", want: ClassPermission},
		{name: "permission to run", text: "Do you want me to run /tmp/build?", want: ClassPermission},
		{name: "confirmation", text: "Would you like me to continue?", want: ClassConfirmation},
		{name: "choice", text: "Which option?", want: ClassChoice},
		{name: "clarification", text: "Could you provide the target?", want: ClassClarification},
		{name: "unknown", text: "Is the target ready?", want: ClassUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sink := &memorySink{}
			NewAnalyzer(sink, nil).ObserveOutput(Session{SessionID: tt.name}, []byte(tt.text))
			if len(sink.events) != 1 || sink.events[0].Class != tt.want {
				t.Fatalf("events=%+v, want class %q", sink.events, tt.want)
			}
		})
	}
}

func TestDecisionClassesAndIgnoredObservations(t *testing.T) {
	tests := []struct {
		answer string
		want   Decision
	}{
		{answer: "yes\n", want: DecisionAccepted},
		{answer: "no\n", want: DecisionRejected},
		{answer: "Use the staging environment\n", want: DecisionChanged},
		{answer: "maybe\n", want: DecisionUnknown},
	}
	for i, tt := range tests {
		sink := &memorySink{}
		a := NewAnalyzer(sink, nil)
		session := Session{SessionID: fmt.Sprintf("s-%d", i)}
		a.ObserveInput(session, []byte("orphaned input"))
		a.ObserveOutput(session, nil)
		a.ObserveOutput(session, []byte("ordinary output"))
		a.ObserveOutput(session, []byte("Proceed?"))
		a.ObserveInput(session, nil)
		a.ObserveInput(session, []byte(tt.answer))
		if len(sink.events) != 2 || sink.events[1].Decision != tt.want {
			t.Fatalf("answer %q events=%+v, want %q", tt.answer, sink.events, tt.want)
		}
	}
}

func TestFeedbackOrderingJSONAndCanonicalization(t *testing.T) {
	a := NewAnalyzer(errorSink{}, func(text string) string {
		return strings.ReplaceAll(DefaultRedactor(text), "private", "[CUSTOM]")
	})
	questions := []struct {
		session  string
		provider string
		text     string
	}{
		{session: "a1", provider: "codex", text: "Allow private /tmp/run-1 token=one?"},
		{session: "a2", provider: "codex", text: "Allow private /var/run-2 token=two?"},
		{session: "b", provider: "claude", text: "Which option?"},
	}
	for _, question := range questions {
		session := Session{SessionID: question.session, Provider: question.provider}
		a.ObserveOutput(session, []byte(question.text))
		a.ObserveInput(session, []byte("yes"))
	}
	feedback := a.Feedback()
	if len(feedback.Questions) != 2 || feedback.Questions[0].Asked != 2 {
		t.Fatalf("feedback ordering/aggregation=%+v", feedback)
	}
	if strings.Contains(feedback.Questions[0].Example, "private") ||
		strings.Contains(feedback.Questions[0].Example, "one") ||
		strings.Contains(feedback.Questions[0].Example, "two") {
		t.Fatalf("aggregate example not redacted: %q", feedback.Questions[0].Example)
	}
	data, err := feedback.JSON()
	if err != nil || !json.Valid(data) {
		t.Fatalf("Feedback.JSON() data=%q err=%v", data, err)
	}
}

func TestUnicodeTruncationAndConcurrentSessions(t *testing.T) {
	sink := &memorySink{}
	a := NewAnalyzer(sink, nil)
	longQuestion := strings.Repeat("🙂", 2050) + "?"
	a.ObserveOutput(Session{SessionID: "long"}, []byte(longQuestion))
	if got := sink.events[0].Text; !utf8.ValidString(got) || utf8.RuneCountInString(got) != 2048 {
		t.Fatalf("truncated text is invalid or wrong length: valid=%v runes=%d", utf8.ValidString(got), utf8.RuneCountInString(got))
	}

	const sessions = 20
	var wg sync.WaitGroup
	for i := 0; i < sessions; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			session := Session{SessionID: fmt.Sprintf("concurrent-%d", i), Provider: "codex"}
			a.ObserveOutput(session, []byte("Proceed with this command?"))
			a.ObserveInput(session, []byte("yes"))
		}(i)
	}
	wg.Wait()

	feedback := a.Feedback()
	accepted := 0
	for _, stat := range feedback.Questions {
		accepted += stat.Accepted
	}
	if accepted != sessions {
		t.Fatalf("accepted=%d, want %d", accepted, sessions)
	}
}
