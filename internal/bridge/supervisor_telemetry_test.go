package bridge

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/orchael/bridgectl/internal/telemetry"
)

type telemetrySpy struct {
	mu      sync.Mutex
	outputs []telemetryOutput
	inputs  [][]byte
	ended   chan struct{}
}

type telemetryOutput struct {
	stream telemetry.StreamType
	data   []byte
}

func (*telemetrySpy) SessionStarted(telemetry.Session) {}
func (s *telemetrySpy) SessionEnded(telemetry.Session) {
	if s.ended != nil {
		close(s.ended)
	}
}
func (*telemetrySpy) Close(context.Context) error { return nil }
func (s *telemetrySpy) ObserveProviderChunk(_ telemetry.Session, stream telemetry.StreamType, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outputs = append(s.outputs, telemetryOutput{stream: stream, data: bytes.Clone(data)})
}

func TestSupervisorEndsTelemetryAfterReaderDrains(t *testing.T) {
	spy := &telemetrySpy{ended: make(chan struct{})}
	sup := NewSupervisor(NewRegistry(), DefaultPolicy(), 1024, time.Minute, WithTelemetry(spy))
	defer sup.Close()
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ms := &managedSession{
		info:       SessionInfo{SessionID: "reader-order", Provider: "test", State: SessionStateRunning},
		cmd:        cmd,
		buf:        NewByteBuffer(1024),
		cancel:     func() {},
		readerDone: make(chan struct{}),
	}
	go sup.waitLoop(ms)
	select {
	case <-spy.ended:
		t.Fatal("telemetry ended before the output reader drained")
	case <-time.After(50 * time.Millisecond):
	}
	close(ms.readerDone)
	select {
	case <-spy.ended:
	case <-time.After(time.Second):
		t.Fatal("telemetry did not end after the output reader drained")
	}
}
func (s *telemetrySpy) ObserveInputChunk(_ telemetry.Session, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inputs = append(s.inputs, bytes.Clone(data))
}

type bufferWriteCloser struct{ bytes.Buffer }

func (*bufferWriteCloser) Close() error { return nil }

type orderedTelemetrySpy struct {
	mu           sync.Mutex
	events       []string
	inputStarted chan struct{}
	releaseInput chan struct{}
}

func (*orderedTelemetrySpy) SessionStarted(telemetry.Session) {}
func (*orderedTelemetrySpy) ObserveProviderChunk(telemetry.Session, telemetry.StreamType, []byte) {
}
func (s *orderedTelemetrySpy) ObserveInputChunk(telemetry.Session, []byte) {
	close(s.inputStarted)
	<-s.releaseInput
	s.mu.Lock()
	s.events = append(s.events, "input")
	s.mu.Unlock()
}
func (s *orderedTelemetrySpy) SessionEnded(telemetry.Session) {
	s.mu.Lock()
	s.events = append(s.events, "ended")
	s.mu.Unlock()
}
func (*orderedTelemetrySpy) Close(context.Context) error { return nil }

func TestSupervisorNeverRecordsInputAfterSessionEnded(t *testing.T) {
	spy := &orderedTelemetrySpy{inputStarted: make(chan struct{}), releaseInput: make(chan struct{})}
	sup := NewSupervisor(NewRegistry(), DefaultPolicy(), 1024, time.Minute, WithTelemetry(spy))
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stdin := &bufferWriteCloser{}
	ms := &managedSession{
		info: SessionInfo{SessionID: "ordered", Provider: "codex", State: SessionStateRunning, ActiveWriterClientID: "writer"},
		cmd:  cmd, stdin: stdin, streamJSON: true, buf: NewByteBuffer(1024), cancel: func() {},
		readerDone: make(chan struct{}), observers: make(map[string]*observerEntry),
	}
	close(ms.readerDone)
	sup.sessions["ordered"] = ms
	writeDone := make(chan error, 1)
	go func() {
		_, err := sup.WriteInput("ordered", "writer", []byte("yes\n"))
		writeDone <- err
	}()
	<-spy.inputStarted
	waitDone := make(chan struct{})
	go func() {
		sup.waitLoop(ms)
		close(waitDone)
	}()
	close(spy.releaseInput)
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	<-waitDone
	spy.mu.Lock()
	defer spy.mu.Unlock()
	if len(spy.events) != 2 || spy.events[0] != "input" || spy.events[1] != "ended" {
		t.Fatalf("telemetry order=%v, want input then ended", spy.events)
	}
}

func TestSupervisorTelemetryObservesOutputAndAuthorizedInput(t *testing.T) {
	spy := &telemetrySpy{}
	sup := NewSupervisor(NewRegistry(), DefaultPolicy(), 1024, time.Minute, WithTelemetry(spy))
	defer sup.Close()
	stdin := &bufferWriteCloser{}
	ms := &managedSession{
		info: SessionInfo{SessionID: "s1", ProjectID: "p1", Provider: "codex", State: SessionStateRunning, ActiveWriterClientID: "writer"},
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
	ms.mu.Lock()
	ms.info.State = SessionStateStopped
	ms.mu.Unlock()
}

func TestSupervisorTelemetrySeesRawANSIWhileClientsSeeStrippedOutput(t *testing.T) {
	spy := &telemetrySpy{}
	sup := NewSupervisor(NewRegistry(), DefaultPolicy(), 1024, time.Minute, WithTelemetry(spy))
	ms := &managedSession{
		info: SessionInfo{SessionID: "ansi", Provider: "codex", State: SessionStateRunning},
		buf:  NewByteBuffer(1024), stripANSI: true, observers: make(map[string]*observerEntry),
	}
	raw := []byte("\x1b[31mhello\x1b[0m")
	sup.appendChunk(ms, raw, ChunkTypeOutput)
	if len(spy.outputs) != 1 || !bytes.Equal(spy.outputs[0].data, raw) {
		t.Fatalf("telemetry output=%q, want original bytes", spy.outputs)
	}
	chunks := ms.buf.After(0)
	if len(chunks) != 1 || string(chunks[0].Payload) != "hello" {
		t.Fatalf("client chunks=%+v, want stripped output", chunks)
	}
}
