package telemetry

import (
	"testing"
	"time"
)

type memorySink struct{ events []Event }

func (s *memorySink) Record(e Event) error {
	s.events = append(s.events, e)
	return nil
}

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
	if got := sink.events[0].Text; got == "Allow this command with token=super-secret?" {
		t.Fatalf("secret was not redacted: %q", got)
	}
}
