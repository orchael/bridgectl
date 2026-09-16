package telemetry

import (
	"sort"
	"time"
	"unicode/utf8"
)

// MaxTurnTextBytes bounds retained text per composite identity.
const MaxTurnTextBytes = 64 * 1024

// Turn is a reconstructed contiguous human or agent interaction, emitted in
// bounded chunks. ChunkIndex is zero for the first chunk of a logical turn;
// Continues marks a size-sealed chunk. Chunks of one oversized event can share
// sequence numbers. ByteCount and Redactions are additive across chunks.
type Turn struct {
	SourceID      string     `json:"source_id,omitempty"`
	SessionID     string     `json:"session_id"`
	ActorID       string     `json:"actor_id,omitempty"`
	Direction     Direction  `json:"direction"`
	Stream        StreamType `json:"stream"`
	SequenceStart uint64     `json:"sequence_start"`
	SequenceEnd   uint64     `json:"sequence_end"`
	StartedAt     time.Time  `json:"started_at"`
	EndedAt       time.Time  `json:"ended_at"`
	Text          string     `json:"text,omitempty"`
	ByteCount     int        `json:"byte_count"`
	Redactions    int        `json:"redactions"`
	ChunkIndex    int        `json:"chunk_index"`
	Continues     bool       `json:"continues,omitempty"`
}

// DataQuality summarizes evidence integrity observed during reconstruction.
type DataQuality struct {
	Events           int `json:"events"`
	Duplicates       int `json:"duplicates"`
	OutOfOrder       int `json:"out_of_order"`
	MissingSequences int `json:"missing_sequences"`
	LegacyEvents     int `json:"legacy_events"`
	OmittedEvents    int `json:"omitted_events"`
	Redactions       int `json:"redactions"`
}

// InteractionAnalyzer incrementally reconstructs turns without retaining the
// entire raw event corpus.
type InteractionAnalyzer struct {
	lastSequence map[sessionIdentity]uint64
	pending      map[sessionIdentity]Turn
	starts       map[sessionIdentity]sessionStart
	quality      DataQuality
}

type sessionStart struct {
	sequence  uint64
	timestamp time.Time
}

func NewInteractionAnalyzer() *InteractionAnalyzer {
	return &InteractionAnalyzer{lastSequence: make(map[sessionIdentity]uint64), pending: make(map[sessionIdentity]Turn), starts: make(map[sessionIdentity]sessionStart)}
}

// Observe consumes one event and returns any turns completed by that event.
func (a *InteractionAnalyzer) Observe(event Event) []Turn {
	a.quality.Events++
	if event.SchemaVersion == 1 {
		a.quality.LegacyEvents++
	}
	a.quality.Redactions += event.Redactions
	if event.OmittedReason != "" {
		a.quality.OmittedEvents++
	}
	key := eventKey(event)
	var completed []Turn
	if event.Kind == EventSessionStarted {
		previous, exists := a.starts[key]
		if exists && previous.sequence == event.Sequence && previous.timestamp.Equal(event.Timestamp) {
			a.quality.Duplicates++
			return nil
		}
		if exists && !event.Timestamp.IsZero() && event.Timestamp.Before(previous.timestamp) {
			a.quality.OutOfOrder++
			return nil
		}
		completed = a.flush(key)
		delete(a.lastSequence, key)
		a.starts[key] = sessionStart{sequence: event.Sequence, timestamp: event.Timestamp}
	}
	last := a.lastSequence[key]
	if event.Sequence > 0 && last > 0 {
		switch {
		case event.Sequence == last:
			a.quality.Duplicates++
			return nil
		case event.Sequence < last:
			a.quality.OutOfOrder++
			return nil
		case event.Sequence > last+1:
			a.quality.MissingSequences += int(event.Sequence - last - 1)
			completed = append(completed, a.flush(key)...)
		}
	}
	if event.Sequence > 0 {
		a.lastSequence[key] = event.Sequence
	}
	if event.Kind == EventSessionEnded {
		return append(completed, a.flush(key)...)
	}
	if event.Kind != EventProviderOutput && event.Kind != EventUserInput {
		return completed
	}
	pending, ok := a.pending[key]
	if ok && (pending.Direction != event.Direction || pending.Stream != event.Stream) {
		completed = append(completed, a.flush(key)...)
		ok = false
	}
	if !ok {
		a.startTurn(key, event)
	}
	return append(completed, a.appendEvent(key, event)...)
}

func (a *InteractionAnalyzer) startTurn(key sessionIdentity, event Event) {
	a.pending[key] = Turn{
		SourceID: event.SourceID, SessionID: event.SessionID, ActorID: event.ActorID,
		Direction: event.Direction, Stream: event.Stream, SequenceStart: event.Sequence, SequenceEnd: event.Sequence,
		StartedAt: event.Timestamp, EndedAt: event.Timestamp,
	}
}

func (a *InteractionAnalyzer) appendEvent(key sessionIdentity, event Event) []Turn {
	var completed []Turn
	remaining := event.Text
	consumed, allocated := 0, 0
	for {
		pending := a.pending[key]
		capacity := MaxTurnTextBytes - len(pending.Text)
		n := len(remaining)
		if n > capacity {
			n = capacity
		}
		for n > 0 && n < len(remaining) && !utf8.RuneStart(remaining[n]) {
			n--
		}
		if n == 0 && len(remaining) > 0 {
			pending.Continues = true
			completed = append(completed, pending)
			a.startTurn(key, event)
			next := a.pending[key]
			next.ChunkIndex = pending.ChunkIndex + 1
			a.pending[key] = next
			continue
		}
		pending.Text += remaining[:n]
		pending.SequenceEnd = event.Sequence
		pending.EndedAt = event.Timestamp
		consumed += n
		bytes := event.ByteCount
		if consumed < len(event.Text) {
			bytes = int(int64(event.ByteCount) * int64(consumed) / int64(len(event.Text)))
		}
		pending.ByteCount += bytes - allocated
		allocated = bytes
		remaining = remaining[n:]
		if len(remaining) == 0 {
			pending.Redactions += event.Redactions
		}
		a.pending[key] = pending
		if len(remaining) == 0 {
			return completed
		}
	}
}

func (a *InteractionAnalyzer) flush(key sessionIdentity) []Turn {
	turn, ok := a.pending[key]
	if !ok {
		return nil
	}
	delete(a.pending, key)
	return []Turn{turn}
}

// Finish returns incomplete turns in deterministic composite-identity order.
func (a *InteractionAnalyzer) Finish() []Turn {
	keys := make([]sessionIdentity, 0, len(a.pending))
	for key := range a.pending {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].sourceID == keys[j].sourceID {
			return keys[i].sessionID < keys[j].sessionID
		}
		return keys[i].sourceID < keys[j].sourceID
	})
	var turns []Turn
	for _, key := range keys {
		turns = append(turns, a.flush(key)...)
	}
	return turns
}

func (a *InteractionAnalyzer) Quality() DataQuality { return a.quality }
