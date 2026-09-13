package telemetry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// JSONLSink appends one normalized event per line. It deliberately owns no
// retention or upload policy; callers decide where the local telemetry file
// lives and how long it is kept.
type JSONLSink struct {
	mu   sync.Mutex
	path string
	perm os.FileMode
}

func NewJSONLSink(path string) *JSONLSink {
	return &JSONLSink{path: path, perm: 0o600}
}

func (s *JSONLSink) Record(event Event) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, s.perm)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return err
	}
	return nil
}
