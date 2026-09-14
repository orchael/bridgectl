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
	"unicode/utf8"
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
	EventQuestion       EventKind = "question"
	EventAnswer         EventKind = "answer"
	EventSessionStarted EventKind = "session_started"
	EventSessionEnded   EventKind = "session_ended"
	EventProviderOutput EventKind = "provider_output"
	EventUserInput      EventKind = "user_input"
)

// StreamType distinguishes normal provider output, model thinking, and human
// input without coupling telemetry to the bridge transport's chunk enum.
type StreamType string

const (
	StreamOutput   StreamType = "output"
	StreamThinking StreamType = "thinking"
	StreamInput    StreamType = "input"
)

const OmittedInvalidUTF8 = "invalid_utf8"

// QuestionClass describes why an agent appears to be asking for input.
type QuestionClass string

const (
	ClassPermission    QuestionClass = "permission"
	ClassConfirmation  QuestionClass = "confirmation"
	ClassChoice        QuestionClass = "choice"
	ClassClarification QuestionClass = "clarification"
	ClassUnknown       QuestionClass = "unknown"
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
	SchemaVersion int           `json:"schema_version"`
	Timestamp     time.Time     `json:"timestamp"`
	SourceID      string        `json:"source_id,omitempty"`
	SessionID     string        `json:"session_id"`
	ProjectID     string        `json:"project_id,omitempty"`
	Provider      string        `json:"provider,omitempty"`
	Direction     Direction     `json:"direction,omitempty"`
	Kind          EventKind     `json:"kind"`
	Stream        StreamType    `json:"stream,omitempty"`
	Sequence      uint64        `json:"sequence"`
	Class         QuestionClass `json:"class,omitempty"`
	Decision      Decision      `json:"decision,omitempty"`
	Fingerprint   string        `json:"fingerprint,omitempty"`
	Text          string        `json:"text,omitempty"`
	ByteCount     int           `json:"byte_count,omitempty"`
	Redactions    int           `json:"redactions,omitempty"`
	ContentHash   string        `json:"content_sha256,omitempty"`
	OmittedReason string        `json:"omitted_reason,omitempty"`
	LatencyMS     int64         `json:"latency_ms,omitempty"`
}

// Session identifies an agent session without requiring telemetry to depend on
// the bridge package.
type Session struct {
	SourceID  string
	SessionID string
	ProjectID string
	Provider  string
}

// Sink synchronously receives normalized telemetry events. Analyzer ignores
// sink errors; live integrations must also move Record off provider I/O paths.
type Sink interface {
	Record(Event) error
}

// Redactor removes secrets and other sensitive material before persistence.
type Redactor func(string) string

// Analyzer correlates agent questions with subsequent human input.
type Analyzer struct {
	mu                  sync.Mutex
	sink                Sink
	redact              Redactor
	pending             map[sessionIdentity]pendingQuestion
	stats               map[string]*QuestionStat
	now                 func() time.Time
	includeRedactedText bool
	sequenceMu          sync.Mutex
	sequences           map[sessionIdentity]uint64
}

type sessionIdentity struct {
	sourceID  string
	sessionID string
}

func identity(sourceID, sessionID string) sessionIdentity {
	return sessionIdentity{sourceID: sourceID, sessionID: sessionID}
}

func sessionKey(session Session) sessionIdentity {
	return identity(session.SourceID, session.SessionID)
}

func eventKey(event Event) sessionIdentity {
	return identity(event.SourceID, event.SessionID)
}

type AnalyzerOption func(*Analyzer)

func WithIncludeRedactedText(include bool) AnalyzerOption {
	return func(a *Analyzer) { a.includeRedactedText = include }
}

type pendingQuestion struct {
	askedAt     time.Time
	class       QuestionClass
	fingerprint string
}

// QuestionStat is the provider-neutral aggregate for one recurring question.
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

// Feedback is a stable, provider-neutral aggregate export. Higher-level
// analysis can turn it into distilled findings for policy systems.
type Feedback struct {
	SchemaVersion int            `json:"schema_version"`
	GeneratedAt   time.Time      `json:"generated_at"`
	Questions     []QuestionStat `json:"questions"`
}

var (
	ansiRE            = regexp.MustCompile(`\x1b(?:\[[0-9;?=<>]*[a-zA-Z~]|[@-Z\\-_])`)
	secretRE          = regexp.MustCompile(`(?i)\b(?:[a-z0-9]+[_-])*(?:api[_-]?key|token|password|secret(?:[_-]access[_-]key)?|authorization)\s*[:=]\s*(?:bearer\s+)?[^\s]+`)
	bearerRE          = regexp.MustCompile(`(?i)\bbearer\s+[^\s]+`)
	credentialValueRE = regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{12,}|ghp_[A-Za-z0-9]{12,}|github_pat_[A-Za-z0-9_]{12,}|AKIA[A-Z0-9]{16}|xox[baprs]-[A-Za-z0-9-]{10,})\b`)
	privateKeyRE      = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)
	numberRE          = regexp.MustCompile(`\b[0-9]+\b`)
	pathRE            = regexp.MustCompile(`(?:/[^\s]+)+`)
	permissionRE      = regexp.MustCompile(`(?i)\b(permission|allow|approve|run this|(?:do you want me to|would you like me to|should i|shall i) run|execute|proceed with (?:this )?command|use this tool)\b`)
	confirmationRE    = regexp.MustCompile(`(?i)\b(continue|proceed|are you sure|shall i|should i|would you like me to|do you want me to)\b`)
	choiceRE          = regexp.MustCompile(`(?i)\b(which|choose|select|option|pick one)\b`)
	clarificationRE   = regexp.MustCompile(`(?i)\b(clarify|what do you mean|need more information|could you provide|please specify)\b`)
	acceptRE          = regexp.MustCompile(`(?i)^\s*(y|yes|ok|okay|sure|approve|approved|allow|allowed|continue|proceed|run it|go ahead|1)\s*[\r\n]*$`)
	rejectRE          = regexp.MustCompile(`(?i)^\s*(n|no|deny|denied|reject|rejected|stop|cancel|2)\s*[\r\n]*$`)
)

func NewAnalyzer(sink Sink, redactor Redactor, opts ...AnalyzerOption) *Analyzer {
	if redactor == nil {
		redactor = DefaultRedactor
	}
	a := &Analyzer{
		sink:                sink,
		redact:              redactor,
		pending:             make(map[sessionIdentity]pendingQuestion),
		stats:               make(map[string]*QuestionStat),
		sequences:           make(map[sessionIdentity]uint64),
		now:                 time.Now,
		includeRedactedText: true,
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

func (a *Analyzer) ObserveSessionStart(session Session) {
	a.sequenceMu.Lock()
	delete(a.sequences, sessionKey(session))
	a.sequenceMu.Unlock()
	a.record(Event{Timestamp: a.now().UTC(), SourceID: session.SourceID, SessionID: session.SessionID, ProjectID: session.ProjectID, Provider: session.Provider, Kind: EventSessionStarted})
}

func (a *Analyzer) ObserveSessionEnd(session Session) {
	a.mu.Lock()
	delete(a.pending, sessionKey(session))
	a.mu.Unlock()
	a.record(Event{Timestamp: a.now().UTC(), SourceID: session.SourceID, SessionID: session.SessionID, ProjectID: session.ProjectID, Provider: session.Provider, Kind: EventSessionEnded})
}

// DefaultRedactor strips ANSI controls and common inline secret assignments.
func DefaultRedactor(text string) string {
	text = ansiRE.ReplaceAllString(text, "")
	text = privateKeyRE.ReplaceAllString(text, "[REDACTED:PRIVATE_KEY]")
	text = bearerRE.ReplaceAllString(text, "Bearer [REDACTED:TOKEN]")
	text = credentialValueRE.ReplaceAllString(text, "[REDACTED:TOKEN]")
	return secretRE.ReplaceAllString(text, "$1=[REDACTED:SECRET]")
}

// ObserveProviderInteraction records a complete provider chunk before logical
// framing so a selected full-capture stream can be reconstructed in order.
func (a *Analyzer) ObserveProviderInteraction(session Session, stream StreamType, data []byte) {
	a.observeInteraction(session, DirectionAgent, EventProviderOutput, stream, data)
}

// ObserveUserInteraction records only input that already passed the bridge's
// active-writer authorization check.
func (a *Analyzer) ObserveUserInteraction(session Session, data []byte) {
	a.observeInteraction(session, DirectionHuman, EventUserInput, StreamInput, data)
}

func (a *Analyzer) observeInteraction(session Session, direction Direction, kind EventKind, stream StreamType, data []byte) {
	if len(data) == 0 {
		return
	}
	event := Event{
		Timestamp: a.now().UTC(), SourceID: session.SourceID, SessionID: session.SessionID, ProjectID: session.ProjectID,
		Provider: session.Provider, Direction: direction, Kind: kind, Stream: stream, ByteCount: len(data),
	}
	if !utf8.Valid(data) {
		sum := sha256.Sum256(data)
		event.ContentHash = hex.EncodeToString(sum[:])
		event.OmittedReason = OmittedInvalidUTF8
		a.record(event)
		return
	}
	redacted := a.redact(string(data))
	event.Redactions = strings.Count(redacted, "[REDACTED:")
	event.Text = a.eventText(redacted)
	a.record(event)
}

// ObserveOutput inspects agent output. It records only likely questions rather
// than every output byte, which keeps the feedback dataset useful and bounded.
func (a *Analyzer) ObserveOutput(session Session, data []byte) {
	text := normalize(string(data))
	if text == "" || !looksLikeQuestion(text) {
		return
	}

	class := classifyQuestion(text)
	now := a.now().UTC()
	redacted := truncate(a.redact(text), 2048)
	canonical := canonicalize(redacted)
	fingerprint := fingerprint(session.Provider, class, canonical)

	a.mu.Lock()
	key := sessionKey(session)
	if pending, ok := a.pending[key]; ok && pending.fingerprint == fingerprint {
		a.mu.Unlock()
		return
	}
	a.pending[key] = pendingQuestion{
		askedAt: now, class: class, fingerprint: fingerprint,
	}
	stat := a.stats[fingerprint]
	if stat == nil {
		stat = &QuestionStat{Fingerprint: fingerprint, Class: class}
		if a.includeRedactedText {
			stat.Example = redacted
		}
		a.stats[fingerprint] = stat
	}
	stat.Asked++
	a.mu.Unlock()

	a.record(Event{
		Timestamp: now, SourceID: session.SourceID, SessionID: session.SessionID, ProjectID: session.ProjectID,
		Provider: session.Provider, Direction: DirectionAgent, Kind: EventQuestion,
		Class: class, Fingerprint: fingerprint, Text: a.eventText(redacted),
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
	key := sessionKey(session)
	pending, ok := a.pending[key]
	if !ok {
		a.mu.Unlock()
		return
	}
	delete(a.pending, key)
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
		Timestamp: now, SourceID: session.SourceID, SessionID: session.SessionID, ProjectID: session.ProjectID,
		Provider: session.Provider, Direction: DirectionHuman, Kind: EventAnswer,
		Class: pending.class, Decision: decision, Fingerprint: pending.fingerprint,
		Text: a.eventText(truncate(a.redact(answer), 2048)), LatencyMS: now.Sub(pending.askedAt).Milliseconds(),
	})
}

func (a *Analyzer) eventText(redacted string) string {
	if !a.includeRedactedText {
		return ""
	}
	return redacted
}

// Feedback returns aggregate data sorted by frequency. Higher-level analysis
// does not need raw transcripts to identify recurring questions.
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
	a.sequenceMu.Lock()
	defer a.sequenceMu.Unlock()
	event.SchemaVersion = 1
	key := eventKey(event)
	a.sequences[key]++
	event.Sequence = a.sequences[key]
	if a.sink != nil {
		_ = a.sink.Record(event)
	}
	if event.Kind == EventSessionEnded {
		delete(a.sequences, key)
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
	text = numberRE.ReplaceAllString(text, "#")
	text = pathRE.ReplaceAllString(text, "<path>")
	return truncate(strings.Join(strings.Fields(text), " "), 512)
}

func fingerprint(provider string, class QuestionClass, text string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(provider) + "\x00" + string(class) + "\x00" + text))
	return hex.EncodeToString(sum[:8])
}

func truncate(text string, max int) string {
	runes := []rune(text)
	if len(runes) <= max {
		return text
	}
	return string(runes[:max])
}
