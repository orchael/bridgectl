package telemetry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Direction identifies which side of an agent session produced an event.
type Direction string

const (
	DirectionAgent Direction = "agent"
	DirectionHuman Direction = "human"
)

// EventKind is the normalized telemetry event type.
type EventKind string

const (
	EventQuestion EventKind = "question"
	EventAnswer   EventKind = "answer"
)

// QuestionClass describes why an agent appears to be asking for input.
type QuestionClass string

const (
	ClassPermission   QuestionClass = "permission"
	ClassConfirmation QuestionClass = "confirmation"
	ClassChoice       QuestionClass = "choice"
	ClassClarification QuestionClass = "clarification"
	ClassUnknown      QuestionClass = "unknown"
)

// Decision is the normalized outcome of a question.
type Decision string

const (
	DecisionAccepted Decision = "accepted"
	DecisionRejected Decision = "rejected"
	DecisionChanged  Decision = "changed"
	DecisionUnknown  Decision = "unknown"
)

// Event is safe to persist after Redactor has processed its text fields.
type Event struct {
	Timestamp   time.Time     `json:"timestamp"`
	SessionID   string        `json:"session_id"`
	ProjectID   string        `json:"project_id,omitempty"`
	Provider    string        `json:"provider,omitempty"`
	Direction   Direction     `json:"direction"`
	Kind        EventKind     `json:"kind"`
	Class       QuestionClass `json:"class,omitempty"`
	Decision    Decision      `json:"decision,omitempty"`
	Fingerprint string        `json:"fingerprint,omitempty"`
	Text        string        `json:"text,omitempty"`
	LatencyMS   int64         `json:"latency_ms,omitempty"`
}

// Session identifies an agent session without requiring telemetry to depend on
// the bridge package.
type Session struct {
	SessionID string
	ProjectID string
	Provider  string
}

// Sink receives normalized telemetry events. Implementations should make
// recording best-effort: telemetry must never block an agent session.
type Sink interface {
	Record(Event) error
}

// Redactor removes secrets and other sensitive material before persistence.
type Redactor func(string) string

// Analyzer correlates agent questions with subsequent human input.
type Analyzer struct {
	mu      sync.Mutex
	sink    Sink
	redact  Redactor
	pending map[string]pendingQuestion
	stats   map[string]*QuestionStat
	now     func() time.Time
}

type pendingQuestion struct {
	session     Session
	askedAt     time.Time
	class       QuestionClass
	fingerprint string
	text        string
}

// QuestionStat is the aggregate used to decide which prompts should be removed
// by Ballast policy rather than auto-approved blindly.
type QuestionStat struct {
	Fingerprint string        `json:"fingerprint"`
	Class       QuestionClass `json:"class"`
	Example     string        `json:"example"`
	Asked       int           `json:"asked"`
	Accepted    int           `json:"accepted"`
	Rejected    int           `json:"rejected"`
	Changed     int           `json:"changed"`
	Unknown     int           `json:"unknown"`
}

// Feedback is a stable, provider-neutral export that Ballast can consume.
type Feedback struct {
	SchemaVersion int            `json:"schema_version"`
	GeneratedAt   time.Time      `json:"generated_at"`
	Questions     []QuestionStat `json:"questions"`
}

var (
	ansiRE = regexp.MustCompile(`\x1b(?:\[[0-9;?=<>]*[a-zA-Z~]|[@-Z\\-_])`)
	secretRE = regexp.MustCompile(`(?i)(api[_-]?key|token|password|secret|authorization)\s*[:=]\s*[^\s]+`)
	permissionRE = regexp.MustCompile(`(?i)\b(permission|allow|approve|run this|execute|proceed with (?:this )?command|use this tool)\b`)
	confirmationRE = regexp.MustCompile(`(?i)\b(continue|proceed|are you sure|shall i|should i|would you like me to|do you want me to)\b`)
	choiceRE = regexp.MustCompile(`(?i)\b(which|choose|select|option|pick one)\b`)
	clarificationRE = regexp.MustCompile(`(?i)\b(clarify|what do you mean|need more information|could you provide|please specify)\b`)
	acceptRE = regexp.MustCompile(`(?i)^\s*(y|yes|ok|okay|sure|approve|approved|allow|allowed|continue|proceed|run it|go ahead|1)\s*[\r\n]*$`)
	rejectRE = regexp.MustCompile(`(?i)^\s*(n|no|deny|denied|reject|rejected|stop|cancel|2)\s*[\r\n]*$`)
)

func NewAnalyzer(sink Sink, redactor Redactor) *Analyzer {
	if redactor == nil {
		redactor = DefaultRedactor
	}
	return &Analyzer{
		sink:    sink,
		redact:  redactor,
		pending: make(map[string]pendingQuestion),
		stats:   make(map[string]*QuestionStat),
		now:     time.Now,
	}
}

// DefaultRedactor strips ANSI controls and common inline secret assignments.
func DefaultRedactor(text string) string {
	text = ansiRE.ReplaceAllString(text, "")
	return secretRE.ReplaceAllString(text, "$1=[REDACTED]")
}

// ObserveOutput inspects agent output. It records only likely questions rather
// than every output byte, which keeps the feedback dataset useful and bounded.
func (a *Analyzer) ObserveOutput(session Session, data []byte) {
	text := normalize(string(data))
	if text == "" || !looksLikeQuestion(text) {
		return
	}

	class := classifyQuestion(text)
	canonical := canonicalize(text)
	fingerprint := fingerprint(session.Provider, class, canonical)
	now := a.now().UTC()
	redacted := truncate(a.redact(text), 2048)

	a.mu.Lock()
	a.pending[session.SessionID] = pendingQuestion{
		session: session, askedAt: now, class: class,
		fingerprint: fingerprint, text: redacted,
	}
	stat := a.stats[fingerprint]
	if stat == nil {
		stat = &QuestionStat{Fingerprint: fingerprint, Class: class, Example: redacted}
		a.stats[fingerprint] = stat
	}
	stat.Asked++
	a.mu.Unlock()

	a.record(Event{
		Timestamp: now, SessionID: session.SessionID, ProjectID: session.ProjectID,
		Provider: session.Provider, Direction: DirectionAgent, Kind: EventQuestion,
		Class: class, Fingerprint: fingerprint, Text: redacted,
	})
}

// ObserveInput correlates human input with the most recent question in the
// session. The raw answer is redacted before it reaches the sink.
func (a *Analyzer) ObserveInput(session Session, data []byte) {
	now := a.now().UTC()
	answer := normalize(string(data))
	if answer == "" {
		return
	}

	a.mu.Lock()
	pending, ok := a.pending[session.SessionID]
	if !ok {
		a.mu.Unlock()
		return
	}
	delete(a.pending, session.SessionID)
	decision := classifyDecision(answer)
	stat := a.stats[pending.fingerprint]
	if stat != nil {
		switch decision {
		case DecisionAccepted:
			stat.Accepted++
		case DecisionRejected:
			stat.Rejected++
		case DecisionChanged:
			stat.Changed++
		default:
			stat.Unknown++
		}
	}
	a.mu.Unlock()

	a.record(Event{
		Timestamp: now, SessionID: session.SessionID, ProjectID: session.ProjectID,
		Provider: session.Provider, Direction: DirectionHuman, Kind: EventAnswer,
		Class: pending.class, Decision: decision, Fingerprint: pending.fingerprint,
		Text: truncate(a.redact(answer), 2048), LatencyMS: now.Sub(pending.askedAt).Milliseconds(),
	})
}

// Feedback returns aggregate data sorted by frequency. Raw transcripts are not
// required for the Ballast feedback loop.
func (a *Analyzer) Feedback() Feedback {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := Feedback{SchemaVersion: 1, GeneratedAt: a.now().UTC()}
	for _, stat := range a.stats {
		out.Questions = append(out.Questions, *stat)
	}
	sort.Slice(out.Questions, func(i, j int) bool {
		if out.Questions[i].Asked == out.Questions[j].Asked {
			return out.Questions[i].Fingerprint < out.Questions[j].Fingerprint
		}
		return out.Questions[i].Asked > out.Questions[j].Asked
	})
	return out
}

func (f Feedback) JSON() ([]byte, error) {
	return json.MarshalIndent(f, "", "  ")
}

func (a *Analyzer) record(event Event) {
	if a.sink != nil {
		_ = a.sink.Record(event)
	}
}

func normalize(text string) string {
	text = ansiRE.ReplaceAllString(text, "")
	text = strings.ReplaceAll(text, "\r", "\n")
	text = strings.Join(strings.Fields(text), " ")
	return strings.TrimSpace(text)
}

func looksLikeQuestion(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(text, "?") ||
		permissionRE.MatchString(lower) || confirmationRE.MatchString(lower) ||
		choiceRE.MatchString(lower) || clarificationRE.MatchString(lower)
}

func classifyQuestion(text string) QuestionClass {
	switch {
	case permissionRE.MatchString(text):
		return ClassPermission
	case confirmationRE.MatchString(text):
		return ClassConfirmation
	case choiceRE.MatchString(text):
		return ClassChoice
	case clarificationRE.MatchString(text):
		return ClassClarification
	default:
		return ClassUnknown
	}
}

func classifyDecision(text string) Decision {
	switch {
	case acceptRE.MatchString(text):
		return DecisionAccepted
	case rejectRE.MatchString(text):
		return DecisionRejected
	case len(strings.Fields(text)) > 2:
		return DecisionChanged
	default:
		return DecisionUnknown
	}
}

func canonicalize(text string) string {
	text = strings.ToLower(text)
	text = regexp.MustCompile(`\b[0-9]+\b`).ReplaceAllString(text, "#")
	text = regexp.MustCompile(`(?:/[^\s]+)+`).ReplaceAllString(text, "<path>")
	return truncate(strings.Join(strings.Fields(text), " "), 512)
}

func fingerprint(provider string, class QuestionClass, text string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(provider) + "\x00" + string(class) + "\x00" + text))
	return hex.EncodeToString(sum[:8])
}

func truncate(text string, max int) string {
	if len(text) <= max {
		return text
	}
	return text[:max]
}
