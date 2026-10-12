package bridge

import (
	"sync"
	"testing"
	"time"
)

type blockingChunkStore struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingChunkStore) Save(SessionInfo) error                   { return nil }
func (s *blockingChunkStore) LoadAll() ([]SessionInfo, error)          { return nil, nil }
func (s *blockingChunkStore) LoadChunks(string) ([]OutputChunk, error) { return nil, nil }
func (s *blockingChunkStore) Close() error                             { return nil }
func (s *blockingChunkStore) SaveChunk(string, OutputChunk) error {
	s.once.Do(func() {
		close(s.entered)
		<-s.release
	})
	return nil
}

func TestAppendChunkOrdersConcurrentProducersAndClose(t *testing.T) {
	store := &blockingChunkStore{entered: make(chan struct{}), release: make(chan struct{})}
	live := make(chan OutputChunk, 4)
	ms := &managedSession{
		info:      SessionInfo{SessionID: "ordered-output"},
		buf:       NewByteBuffer(1024),
		observers: map[string]*observerEntry{"reader": {ch: live}},
	}
	sup := &Supervisor{store: store}
	firstDone := make(chan struct{})
	go func() {
		sup.appendChunk(ms, []byte("first"), ChunkTypeOutput)
		close(firstDone)
	}()
	select {
	case <-store.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first chunk did not reach persistence")
	}
	secondDone := make(chan struct{})
	go func() {
		sup.appendChunk(ms, []byte("second"), ChunkTypeOutput)
		close(secondDone)
	}()
	closeDone := make(chan struct{})
	go func() {
		<-secondDone
		sup.closeLive(ms)
		close(closeDone)
	}()
	select {
	case <-secondDone:
		t.Fatal("second chunk overtook the first persistence write")
	case <-time.After(20 * time.Millisecond):
	}
	close(store.release)
	<-firstDone
	<-secondDone
	<-closeDone
	first, ok := <-live
	if !ok || first.Seq != 1 || string(first.Payload) != "first" {
		t.Fatalf("first live chunk = %+v, open=%v", first, ok)
	}
	second, ok := <-live
	if !ok || second.Seq != 2 || string(second.Payload) != "second" {
		t.Fatalf("second live chunk = %+v, open=%v", second, ok)
	}
	sup.appendChunk(ms, []byte("late"), ChunkTypeOutput)
	if got := ms.buf.LastSeq(); got != 2 {
		t.Fatalf("sequence advanced after close: %d", got)
	}
}
