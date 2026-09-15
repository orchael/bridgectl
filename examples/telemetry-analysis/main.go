// Command telemetry-analysis demonstrates deterministic metrics, read-only
// analytics APIs, and a bounded packet suitable for a separately governed LLM.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/orchael/bridgectl/internal/telemetry"
)

const (
	maxPacketTurns = 20
	maxPacketChars = 4000
)

type Summary struct {
	Sessions   int                   `json:"sessions"`
	AgentTurns int                   `json:"agent_turns"`
	HumanTurns int                   `json:"human_turns"`
	Quality    telemetry.DataQuality `json:"data_quality"`
}

type Finding struct {
	ID       string   `json:"id"`
	Severity string   `json:"severity"`
	Title    string   `json:"title"`
	Evidence []string `json:"evidence"`
}

type SessionView struct {
	SourceID  string                    `json:"source_id,omitempty"`
	SessionID string                    `json:"session_id"`
	Context   *telemetry.SessionContext `json:"context,omitempty"`
	Turns     []telemetry.Turn          `json:"turns"`
}

type Dataset struct {
	Summary  Summary       `json:"summary"`
	Findings []Finding     `json:"findings"`
	Sessions []SessionView `json:"sessions"`
}

type LLMPacket struct {
	Purpose   string           `json:"purpose"`
	Turns     []telemetry.Turn `json:"turns"`
	Truncated bool             `json:"truncated"`
}

func analyze(path string) (Dataset, error) {
	analyzer := telemetry.NewInteractionAnalyzer()
	sessions := make(map[string]*SessionView)
	var completed []telemetry.Turn
	err := telemetry.VisitEvents(path, func(event telemetry.Event) error {
		key := compositeKey(event.SourceID, event.SessionID)
		view := sessions[key]
		if view == nil {
			view = &SessionView{SourceID: event.SourceID, SessionID: event.SessionID}
			sessions[key] = view
		}
		if event.Kind == telemetry.EventSessionContext {
			view.Context = event.Context
		}
		completed = append(completed, analyzer.Observe(event)...)
		return nil
	})
	if err != nil {
		return Dataset{}, err
	}
	completed = append(completed, analyzer.Finish()...)
	for _, turn := range completed {
		key := compositeKey(turn.SourceID, turn.SessionID)
		view := sessions[key]
		if view == nil {
			view = &SessionView{SourceID: turn.SourceID, SessionID: turn.SessionID}
			sessions[key] = view
		}
		view.Turns = append(view.Turns, turn)
	}
	dataset := Dataset{Summary: Summary{Sessions: len(sessions), Quality: analyzer.Quality()}}
	keys := make([]string, 0, len(sessions))
	for key := range sessions {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		view := sessions[key]
		for _, turn := range view.Turns {
			if turn.Stream == telemetry.StreamThinking || turn.ChunkIndex > 0 {
				continue
			}
			if turn.Direction == telemetry.DirectionAgent {
				dataset.Summary.AgentTurns++
			} else {
				dataset.Summary.HumanTurns++
			}
		}
		dataset.Sessions = append(dataset.Sessions, *view)
	}
	dataset.Findings = deterministicFindings(dataset)
	return dataset, nil
}

func deterministicFindings(dataset Dataset) []Finding {
	var findings []Finding
	if dataset.Summary.Quality.MissingSequences > 0 || dataset.Summary.Quality.OmittedEvents > 0 {
		findings = append(findings, Finding{
			ID: "data-integrity", Severity: "warning", Title: "Interaction evidence is incomplete",
			Evidence: []string{fmt.Sprintf("missing_sequences=%d", dataset.Summary.Quality.MissingSequences), fmt.Sprintf("omitted_events=%d", dataset.Summary.Quality.OmittedEvents)},
		})
	}
	for _, session := range dataset.Sessions {
		humanTurns := 0
		for _, turn := range session.Turns {
			if turn.Direction == telemetry.DirectionHuman && turn.ChunkIndex == 0 {
				humanTurns++
			}
		}
		if humanTurns >= 5 {
			findings = append(findings, Finding{ID: "high-intervention:" + compositeKey(session.SourceID, session.SessionID), Severity: "info", Title: "Session needed frequent human intervention", Evidence: []string{fmt.Sprintf("human_turns=%d", humanTurns)}})
		}
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].ID < findings[j].ID })
	return findings
}

func buildLLMPacket(session SessionView) LLMPacket {
	packet := LLMPacket{Purpose: "Suggest interaction-rule or skill improvements from redacted evidence."}
	chars := 0
	for _, turn := range session.Turns {
		if turn.Stream == telemetry.StreamThinking {
			continue
		}
		remaining := maxPacketChars - chars
		if len(packet.Turns) == maxPacketTurns || remaining <= 0 {
			packet.Truncated = true
			break
		}
		turnChars := utf8.RuneCountInString(turn.Text)
		if turnChars > remaining {
			turn.Text = string([]rune(turn.Text)[:remaining])
			turnChars = remaining
			packet.Truncated = true
		}
		chars += turnChars
		packet.Turns = append(packet.Turns, turn)
		if packet.Truncated {
			break
		}
	}
	return packet
}

func routes(dataset Dataset) http.Handler {
	mux := http.NewServeMux()
	jsonHandler := func(value any) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(value)
		}
	}
	mux.HandleFunc("GET /api/analytics/summary", jsonHandler(dataset.Summary))
	mux.HandleFunc("GET /api/analytics/findings", jsonHandler(dataset.Findings))
	mux.HandleFunc("GET /api/analytics/sessions", jsonHandler(dataset.Sessions))
	mux.HandleFunc("GET /api/analytics/sessions/{source}/{session}", func(w http.ResponseWriter, r *http.Request) {
		source, _ := url.PathUnescape(r.PathValue("source"))
		if source == "_legacy" {
			source = ""
		}
		sessionID, _ := url.PathUnescape(r.PathValue("session"))
		for _, session := range dataset.Sessions {
			if session.SourceID == source && session.SessionID == sessionID {
				jsonHandler(session)(w, r)
				return
			}
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("GET /api/analytics/llm-packet/{source}/{session}", func(w http.ResponseWriter, r *http.Request) {
		source := strings.TrimSpace(r.PathValue("source"))
		if source == "_legacy" {
			source = ""
		}
		for _, session := range dataset.Sessions {
			if session.SourceID == source && session.SessionID == r.PathValue("session") {
				jsonHandler(buildLLMPacket(session))(w, r)
				return
			}
		}
		http.NotFound(w, r)
	})
	return mux
}

func compositeKey(sourceID, sessionID string) string { return sourceID + "\x00" + sessionID }

func main() {
	events := flag.String("events", "", "JSONL file or collector segment directory")
	listen := flag.String("listen", "127.0.0.1:9470", "read-only example API address")
	flag.Parse()
	if *events == "" {
		log.Fatal("--events is required")
	}
	dataset, err := analyze(*events)
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{Addr: *listen, Handler: routes(dataset), ReadHeaderTimeout: 5 * time.Second}
	log.Printf("example telemetry analytics listening on http://%s", *listen)
	log.Fatal(server.ListenAndServe())
}
