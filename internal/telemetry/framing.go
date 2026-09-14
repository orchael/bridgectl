package telemetry

import (
	"strings"
	"sync"
)

const defaultFrameBufferSize = 16 << 10

// Framer converts arbitrary terminal chunks into logical line/question frames.
type Framer struct {
	mu       sync.Mutex
	maxBytes int
	outputs  map[string]string
	inputs   map[string]string
}

func NewFramer(maxBytes int) *Framer {
	if maxBytes <= 0 {
		maxBytes = defaultFrameBufferSize
	}
	return &Framer{maxBytes: maxBytes, outputs: make(map[string]string), inputs: make(map[string]string)}
}

func (f *Framer) FeedOutput(sessionID string, data []byte) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	buffer := trimFrameBuffer(f.outputs[sessionID]+string(data), f.maxBytes)
	frames, remainder := splitFrames(buffer, true)
	f.outputs[sessionID] = remainder
	return frames
}

func (f *Framer) FeedInput(sessionID string, data []byte) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	buffer := trimFrameBuffer(f.inputs[sessionID]+string(data), f.maxBytes)
	frames, remainder := splitFrames(buffer, false)
	f.inputs[sessionID] = remainder
	return frames
}

func (f *Framer) FlushOutput(sessionID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	frame := f.outputs[sessionID]
	delete(f.outputs, sessionID)
	return frame
}

func (f *Framer) FlushInput(sessionID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	frame := f.inputs[sessionID]
	delete(f.inputs, sessionID)
	return frame
}

func (f *Framer) Reset(sessionID string) {
	f.mu.Lock()
	delete(f.outputs, sessionID)
	delete(f.inputs, sessionID)
	f.mu.Unlock()
}

func (f *Framer) BufferedOutput(sessionID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.outputs[sessionID]
}

func splitFrames(buffer string, questionMark bool) ([]string, string) {
	var frames []string
	start := 0
	for i := 0; i < len(buffer); i++ {
		if buffer[i] == '\x1b' {
			end, complete := ansiSequenceEnd(buffer, i)
			if !complete {
				break
			}
			i = end - 1
			continue
		}
		if buffer[i] != '\n' && buffer[i] != '\r' && (!questionMark || buffer[i] != '?') {
			continue
		}
		end := i
		if buffer[i] == '?' {
			end++
		}
		if frame := strings.TrimSpace(buffer[start:end]); frame != "" {
			frames = append(frames, frame)
		}
		start = i + 1
	}
	return frames, buffer[start:]
}

func ansiSequenceEnd(buffer string, start int) (int, bool) {
	if start+1 >= len(buffer) {
		return len(buffer), false
	}
	if buffer[start+1] != '[' {
		return start + 2, true
	}
	for i := start + 2; i < len(buffer); i++ {
		// ECMA-48 control sequence terminators occupy 0x40 through 0x7e.
		if buffer[i] >= 0x40 && buffer[i] <= 0x7e {
			return i + 1, true
		}
	}
	return len(buffer), false
}

func trimFrameBuffer(buffer string, maxBytes int) string {
	if len(buffer) <= maxBytes {
		return buffer
	}
	runes := []rune(buffer)
	for len(string(runes)) > maxBytes {
		runes = runes[1:]
	}
	return string(runes)
}
