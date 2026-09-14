package telemetry

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type ReportOptions struct {
	Since time.Time
	Until time.Time
	Top   int
}

type Report struct {
	Since                 time.Time      `json:"since"`
	Until                 time.Time      `json:"until"`
	Sessions              int            `json:"sessions"`
	AgentHours            float64        `json:"agent_hours"`
	Questions             int            `json:"questions"`
	QuestionsPerSession   float64        `json:"questions_per_session"`
	QuestionsPerAgentHour float64        `json:"questions_per_agent_hour"`
	AcceptanceRate        float64        `json:"acceptance_rate"`
	RejectionRate         float64        `json:"rejection_rate"`
	ChangeRate            float64        `json:"change_rate"`
	UnknownRate           float64        `json:"unknown_rate"`
	MedianLatencyMS       int64          `json:"median_latency_ms"`
	TopQuestions          []QuestionStat `json:"top_questions"`
}

func ReadEvents(path string) ([]Event, error) {
	paths, err := retainedEventPaths(path)
	if err != nil {
		return nil, err
	}
	var events []Event
	for _, eventPath := range paths {
		fileEvents, err := readEventFile(eventPath)
		if err != nil {
			return nil, err
		}
		events = append(events, fileEvents...)
	}
	return events, nil
}

func retainedEventPaths(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err == nil && info.IsDir() {
		entries, readErr := os.ReadDir(path)
		if readErr != nil {
			return nil, readErr
		}
		var paths []string
		var active string
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
				continue
			}
			if entry.Name() == activeSegmentName {
				active = filepath.Join(path, entry.Name())
				continue
			}
			id := strings.TrimSuffix(entry.Name(), ".jsonl")
			if validSegmentID(id) {
				paths = append(paths, filepath.Join(path, entry.Name()))
			}
		}
		sort.Strings(paths)
		if active != "" {
			paths = append(paths, active)
		}
		return paths, nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	type rotation struct {
		index int
		path  string
	}
	var rotations []rotation
	baseExists := false
	for _, entry := range entries {
		if entry.Name() == base && !entry.IsDir() {
			baseExists = true
			continue
		}
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), base+".") {
			continue
		}
		index, parseErr := strconv.Atoi(strings.TrimPrefix(entry.Name(), base+"."))
		if parseErr == nil && index > 0 {
			rotations = append(rotations, rotation{index: index, path: filepath.Join(dir, entry.Name())})
		}
	}
	if !baseExists && len(rotations) == 0 {
		_, err := os.Stat(path)
		return nil, err
	}
	sort.Slice(rotations, func(i, j int) bool { return rotations[i].index > rotations[j].index })
	paths := make([]string, 0, len(rotations)+1)
	for _, rotation := range rotations {
		paths = append(paths, rotation.path)
	}
	if baseExists {
		paths = append(paths, path)
	}
	return paths, nil
}

func readEventFile(path string) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4<<20)
	var events []Event
	line := 0
	for scanner.Scan() {
		line++
		var event Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return nil, fmt.Errorf("decode telemetry event %s line %d: %w", path, line, err)
		}
		events = append(events, event)
	}
	return events, scanner.Err()
}

func BuildReport(events []Event, opts ReportOptions) Report {
	until := opts.Until.UTC()
	if until.IsZero() {
		until = time.Now().UTC()
	}
	since := opts.Since.UTC()
	if since.IsZero() {
		since = until.Add(-7 * 24 * time.Hour)
	}
	report := Report{Since: since, Until: until}
	type interval struct{ start, end time.Time }
	intervals := make(map[string]interval)
	sessions := make(map[string]struct{})
	stats := make(map[string]*QuestionStat)
	var latencies []int64
	answers := 0

	for _, event := range events {
		iv := intervals[event.SessionID]
		switch event.Kind {
		case EventSessionStarted:
			if iv.start.IsZero() || event.Timestamp.Before(iv.start) {
				iv.start = event.Timestamp
			}
		case EventSessionEnded:
			if iv.end.IsZero() || event.Timestamp.After(iv.end) {
				iv.end = event.Timestamp
			}
		}
		intervals[event.SessionID] = iv
		if event.Timestamp.Before(since) || event.Timestamp.After(until) {
			continue
		}
		switch event.Kind {
		case EventQuestion:
			sessions[event.SessionID] = struct{}{}
			report.Questions++
			stat := stats[event.Fingerprint]
			if stat == nil {
				stat = &QuestionStat{Fingerprint: event.Fingerprint, Class: event.Class, Example: event.Text}
				stats[event.Fingerprint] = stat
			}
			stat.Asked++
		case EventAnswer:
			answers++
			if event.LatencyMS >= 0 {
				latencies = append(latencies, event.LatencyMS)
			}
			stat := stats[event.Fingerprint]
			if stat == nil {
				stat = &QuestionStat{Fingerprint: event.Fingerprint, Class: event.Class}
				stats[event.Fingerprint] = stat
			}
			switch event.Decision {
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
	}

	for sessionID, iv := range intervals {
		if iv.start.IsZero() {
			continue
		}
		end := iv.end
		if end.IsZero() || end.After(until) {
			end = until
		}
		start := iv.start
		if start.Before(since) {
			start = since
		}
		if end.After(start) {
			sessions[sessionID] = struct{}{}
			report.AgentHours += end.Sub(start).Hours()
		}
	}
	report.Sessions = len(sessions)
	if report.Sessions > 0 {
		report.QuestionsPerSession = float64(report.Questions) / float64(report.Sessions)
	}
	if report.AgentHours > 0 {
		report.QuestionsPerAgentHour = float64(report.Questions) / report.AgentHours
	}
	if answers > 0 {
		for _, stat := range stats {
			report.AcceptanceRate += float64(stat.Accepted)
			report.RejectionRate += float64(stat.Rejected)
			report.ChangeRate += float64(stat.Changed)
			report.UnknownRate += float64(stat.Unknown)
		}
		denom := float64(answers)
		report.AcceptanceRate /= denom
		report.RejectionRate /= denom
		report.ChangeRate /= denom
		report.UnknownRate /= denom
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	if n := len(latencies); n > 0 {
		if n%2 == 1 {
			report.MedianLatencyMS = latencies[n/2]
		} else {
			report.MedianLatencyMS = (latencies[n/2-1] + latencies[n/2]) / 2
		}
	}
	for _, stat := range stats {
		if stat.Asked > 0 {
			report.TopQuestions = append(report.TopQuestions, *stat)
		}
	}
	sort.Slice(report.TopQuestions, func(i, j int) bool {
		if report.TopQuestions[i].Asked == report.TopQuestions[j].Asked {
			return report.TopQuestions[i].Fingerprint < report.TopQuestions[j].Fingerprint
		}
		return report.TopQuestions[i].Asked > report.TopQuestions[j].Asked
	})
	if opts.Top > 0 && len(report.TopQuestions) > opts.Top {
		report.TopQuestions = report.TopQuestions[:opts.Top]
	}
	return report
}

func BuildFeedback(events []Event, since, until time.Time) Feedback {
	if until.IsZero() {
		until = time.Now().UTC()
	}
	report := BuildReport(events, ReportOptions{Since: since, Until: until})
	return Feedback{SchemaVersion: 1, GeneratedAt: until.UTC(), Questions: report.TopQuestions}
}
