package telemetry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// JSONLSink appends one normalized event per line and bounds retained storage
// with size-based rotation. It owns no upload policy.
type JSONLSink struct {
	mu       sync.Mutex
	path     string
	perm     os.FileMode
	maxBytes int64
	maxFiles int
}

func NewJSONLSink(path string) *JSONLSink {
	return NewRotatingJSONLSink(path, 10<<20, 5)
}

func NewRotatingJSONLSink(path string, maxBytes int64, maxFiles int) *JSONLSink {
	if maxBytes < 1 {
		maxBytes = 10 << 20
	}
	if maxFiles < 1 {
		maxFiles = 5
	}
	return &JSONLSink{path: path, perm: 0o600, maxBytes: maxBytes, maxFiles: maxFiles}
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
	if err := s.rotateIfNeeded(int64(len(data) + 1)); err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, s.perm)
	if err != nil {
		return err
	}
	if err := f.Chmod(s.perm); err != nil {
		return errors.Join(err, f.Close())
	}
	_, writeErr := f.Write(append(data, '\n'))
	return errors.Join(writeErr, f.Close())
}

func (s *JSONLSink) rotateIfNeeded(incoming int64) error {
	info, err := os.Stat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Size() == 0 || info.Size()+incoming <= s.maxBytes {
		return nil
	}
	if s.maxFiles == 1 {
		return os.Remove(s.path)
	}
	for index := s.maxFiles - 1; index >= 1; index-- {
		destination := fmt.Sprintf("%s.%d", s.path, index)
		if err := os.Remove(destination); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		source := s.path
		if index > 1 {
			source = fmt.Sprintf("%s.%d", s.path, index-1)
		}
		if err := os.Rename(source, destination); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
