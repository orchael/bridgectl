package bridge

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/orchael/bridgectl/internal/telemetry"
)

type telemetrySpy struct {
	mu      sync.Mutex
	outputs []telemetryOutput
	inputs  [][]byte
}

type telemetryOutput struct {
	stream telemetry.StreamType
	data   []byte
}

func (*telemetrySpy) SessionStarted(telemetry.Session) {}
func (*telemetrySpy) SessionEnded(telemetry.Session)   {}
func (*telemetrySpy) Close(context.Context) error      { return nil }
func (s *telemetrySpy) ObserveProviderChunk(_ telemetry.Session, stream telemetry.StreamType, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outputs = append(s.outputs, telemetryOutput{stream: stream, data: bytes.Clone(data)})
}
func (s *telemetrySpy) ObserveInputChunk(_ telemetry.Session, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inputs = append(s.inputs, bytes.Clone(data))
}

type bufferWriteCloser struct{ bytes.Buffer }

func (*bufferWriteCloser) Close() error { return nil }

func TestSupervisorTelemetryObservesOutputAndAuthorizedInput(t *testing.T) {
	spy := &telemetrySpy{}
	sup := NewSupervisor(NewRegistry(), DefaultPolicy(), 1024, time.Minute, WithTelemetry(spy))
	defer sup.Close()
	stdin := &bufferWriteCloser{}
	ms := &managedSession{
		info: SessionInfo{SessionID: "s1", ProjectID: "p1", Provider: "codex", State: SessionStateStopped, ActiveWriterClientID: "writer"},
		buf:  NewByteBuffer(1024), streamJSON: true, stdin: stdin,
		observers: make(map[string]*observerEntry),
	}
	sup.sessions["s1"] = ms

	sup.appendChunk(ms, []byte("Proceed?"), ChunkTypeOutput)
	sup.appendChunk(ms, []byte("thinking"), ChunkTypeThinking)
	if _, err := sup.WriteInput("s1", "observer", []byte("no\n")); err == nil {
		t.Fatal("unauthorized input succeeded")
	}
	if _, err := sup.WriteInput("s1", "writer", []byte("yes\n")); err != nil {
		t.Fatal(err)
	}

	if len(spy.outputs) != 2 || spy.outputs[0].stream != telemetry.StreamOutput || string(spy.outputs[0].data) != "Proceed?" || spy.outputs[1].stream != telemetry.StreamThinking || string(spy.outputs[1].data) != "thinking" {
		t.Fatalf("outputs=%q", spy.outputs)
	}
	if len(spy.inputs) != 1 || string(spy.inputs[0]) != "yes\n" {
		t.Fatalf("inputs=%q", spy.inputs)
	}
	if got, _ := io.ReadAll(&stdin.Buffer); string(got) != "yes\n" {
		t.Fatalf("provider input=%q", got)
	}
}
