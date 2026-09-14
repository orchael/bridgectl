package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

var (
	ErrInvalidSegmentID = errors.New("invalid telemetry segment ID")
	ErrSegmentConflict  = errors.New("telemetry segment ID has different content")
	ErrSegmentTooLarge  = errors.New("telemetry record exceeds segment size limit")
	segmentIDPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

const activeSegmentName = ".active.jsonl"

// Segment identifies an immutable JSONL file in a SegmentSpool.
type Segment struct {
	ID   string
	Path string
	Size int64
}

// SegmentSpool stores bounded immutable JSONL segments. The active segment is
// private and is atomically renamed when sealed, so a restart can recover it.
type SegmentSpool struct {
	mu              sync.Mutex
	dir             string
	maxSegmentBytes int64
	maxDiskBytes    int64
	onEvict         func(Segment)
	now             func() time.Time
	newID           func() string
}

func NewSegmentSpool(dir string, maxSegmentBytes, maxDiskBytes int64, onEvict func(Segment)) (*SegmentSpool, error) {
	if maxSegmentBytes < 1 || maxDiskBytes < maxSegmentBytes {
		return nil, fmt.Errorf("telemetry spool limits must be positive")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create telemetry spool: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("secure telemetry spool: %w", err)
	}
	s := &SegmentSpool{
		dir: dir, maxSegmentBytes: maxSegmentBytes, maxDiskBytes: maxDiskBytes, onEvict: onEvict,
		now: time.Now, newID: uuid.NewString,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if info, err := os.Stat(s.activePath()); err == nil && info.Size() > 0 {
		if _, err := s.sealLocked(); err != nil {
			return nil, fmt.Errorf("recover telemetry spool: %w", err)
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect telemetry spool: %w", err)
	}
	if err := s.enforceLimitLocked(); err != nil {
		return nil, fmt.Errorf("enforce telemetry spool disk budget: %w", err)
	}
	return s, nil
}

func (s *SegmentSpool) Dir() string { return s.dir }

// Record implements Sink by appending one JSON event to the active segment.
func (s *SegmentSpool) Record(event Event) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	return s.Append(append(data, '\n'))
}

func (s *SegmentSpool) Append(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if int64(len(data)) > s.maxSegmentBytes {
		return ErrSegmentTooLarge
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if info, err := os.Stat(s.activePath()); err == nil && info.Size() > 0 && info.Size()+int64(len(data)) > s.maxSegmentBytes {
		if _, err := s.sealLocked(); err != nil {
			return err
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(s.activePath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		return errors.Join(err, f.Close())
	}
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	return s.enforceLimitLocked()
}

// Seal atomically closes the active segment. An empty Segment means there was
// no active data to seal.
func (s *SegmentSpool) Seal() (Segment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sealLocked()
}

func (s *SegmentSpool) sealLocked() (Segment, error) {
	info, err := os.Stat(s.activePath())
	if errors.Is(err, os.ErrNotExist) {
		return Segment{}, nil
	}
	if err != nil {
		return Segment{}, err
	}
	if info.Size() == 0 {
		if err := os.Remove(s.activePath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return Segment{}, err
		}
		return Segment{}, nil
	}
	id := fmt.Sprintf("%s-%s", s.now().UTC().Format("20060102T150405.000000000Z"), s.newID())
	segment := Segment{ID: id, Path: s.segmentPath(id), Size: info.Size()}
	if err := os.Rename(s.activePath(), segment.Path); err != nil {
		return Segment{}, err
	}
	if err := syncDirectory(s.dir); err != nil {
		return Segment{}, err
	}
	if err := s.enforceLimitLocked(); err != nil {
		return Segment{}, err
	}
	return segment, nil
}

// Accept durably stores a remotely supplied immutable segment. Repeating the
// same ID and bytes is idempotent; reusing an ID for different bytes fails.
func (s *SegmentSpool) Accept(id string, data []byte) (bool, error) {
	if !validSegmentID(id) {
		return false, ErrInvalidSegmentID
	}
	if int64(len(data)) > s.maxSegmentBytes {
		return false, ErrSegmentTooLarge
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.segmentPath(id)
	if existing, err := os.ReadFile(path); err == nil {
		if bytes.Equal(existing, data) {
			return false, nil
		}
		return false, ErrSegmentConflict
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	temp, err := os.CreateTemp(s.dir, ".incoming-*.jsonl")
	if err != nil {
		return false, err
	}
	tempName := temp.Name()
	cleanup := func() { _ = os.Remove(tempName) }
	if err := temp.Chmod(0o600); err != nil {
		cleanup()
		return false, errors.Join(err, temp.Close())
	}
	if _, err := temp.Write(data); err != nil {
		cleanup()
		return false, errors.Join(err, temp.Close())
	}
	if err := temp.Sync(); err != nil {
		cleanup()
		return false, errors.Join(err, temp.Close())
	}
	if err := temp.Close(); err != nil {
		cleanup()
		return false, err
	}
	if err := os.Rename(tempName, path); err != nil {
		cleanup()
		return false, err
	}
	if err := syncDirectory(s.dir); err != nil {
		return false, err
	}
	if err := s.enforceLimitLocked(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *SegmentSpool) Pending() ([]Segment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pendingLocked()
}

func (s *SegmentSpool) pendingLocked() ([]Segment, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	segments := make([]Segment, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" || entry.Name() == activeSegmentName || !validSegmentID(entry.Name()[:len(entry.Name())-len(".jsonl")]) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		id := entry.Name()[:len(entry.Name())-len(".jsonl")]
		segments = append(segments, Segment{ID: id, Path: filepath.Join(s.dir, entry.Name()), Size: info.Size()})
	}
	sort.Slice(segments, func(i, j int) bool { return segments[i].ID < segments[j].ID })
	return segments, nil
}

func (s *SegmentSpool) Read(id string) ([]byte, error) {
	if !validSegmentID(id) {
		return nil, ErrInvalidSegmentID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return os.ReadFile(s.segmentPath(id))
}

func (s *SegmentSpool) Remove(id string) error {
	if !validSegmentID(id) {
		return ErrInvalidSegmentID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.segmentPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirectory(s.dir)
}

func (s *SegmentSpool) Close(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := s.Seal()
	return err
}

func (s *SegmentSpool) enforceLimitLocked() error {
	segments, err := s.pendingLocked()
	if err != nil {
		return err
	}
	var totalBytes int64
	for _, segment := range segments {
		totalBytes += segment.Size
	}
	if info, statErr := os.Stat(s.activePath()); statErr == nil {
		totalBytes += info.Size()
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	for totalBytes > s.maxDiskBytes && len(segments) > 0 {
		oldest := segments[0]
		if err := os.Remove(oldest.Path); err != nil {
			return err
		}
		totalBytes -= oldest.Size
		if s.onEvict != nil {
			s.onEvict(oldest)
		}
		segments = segments[1:]
	}
	return syncDirectory(s.dir)
}

func (s *SegmentSpool) activePath() string { return filepath.Join(s.dir, activeSegmentName) }
func (s *SegmentSpool) segmentPath(id string) string {
	return filepath.Join(s.dir, id+".jsonl")
}
func validSegmentID(id string) bool { return segmentIDPattern.MatchString(id) }

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
