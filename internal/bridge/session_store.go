package bridge

import (
	"bytes"
	"encoding/json"
	"fmt"

	bolt "go.etcd.io/bbolt"
)

var sessionsBucket = []byte("sessions")
var chunksBucket = []byte("chunks")

// SessionStore persists session metadata and PTY output chunks so they
// survive daemon restarts.
type SessionStore interface {
	Save(info SessionInfo) error
	LoadAll() ([]SessionInfo, error)
	SaveChunk(sessionID string, chunk OutputChunk) error
	LoadChunks(sessionID string) ([]OutputChunk, error)
	Close() error
}

// ChunkPageStore supports bounded replay without loading a session into RAM.
// Optional so other SessionStore implementations remain source compatible.
type ChunkPageStore interface {
	LoadChunkPage(sessionID string, after uint64, maxBytes int) ([]OutputChunk, error)
}

func (s *BoltSessionStore) LoadChunkPage(sessionID string, after uint64, maxBytes int) ([]OutputChunk, error) {
	var chunks []OutputChunk
	if after == ^uint64(0) {
		return chunks, nil
	}
	prefix := []byte(sessionID + "/")
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(chunksBucket).Cursor()
		total := 0
		for k, v := c.Seek(chunkKey(sessionID, after+1)); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
			var chunk OutputChunk
			if err := json.Unmarshal(v, &chunk); err != nil {
				return err
			}
			if len(chunks) == 1024 || total+len(chunk.Payload) > maxBytes {
				if len(chunks) == 0 {
					return fmt.Errorf("terminal chunk exceeds replay window")
				}
				break
			}
			chunks = append(chunks, chunk)
			total += len(chunk.Payload)
		}
		return nil
	})
	return chunks, err
}

// BoltSessionStore implements SessionStore using bbolt (an embedded key/value
// store). Each session is stored as a JSON-encoded SessionInfo keyed by its
// session ID inside the "sessions" bucket.
type BoltSessionStore struct {
	db *bolt.DB
}

// NewBoltSessionStore opens (or creates) a bbolt database at path and
// ensures the sessions and chunks buckets exist.
func NewBoltSessionStore(path string) (*BoltSessionStore, error) {
	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		return nil, fmt.Errorf("open session store %q: %w", path, err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(sessionsBucket); err != nil {
			return err
		}
		_, err := tx.CreateBucketIfNotExists(chunksBucket)
		return err
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create store buckets: %w", err)
	}
	return &BoltSessionStore{db: db}, nil
}

// Save writes (or overwrites) the session info for info.SessionID.
func (s *BoltSessionStore) Save(info SessionInfo) error {
	data, err := json.Marshal(info)
	if err != nil {
		return fmt.Errorf("marshal session %q: %w", info.SessionID, err)
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(sessionsBucket).Put([]byte(info.SessionID), data)
	})
}

// chunkKey returns the bbolt key for a chunk: "<sessionID>/<seq-16hex>".
// Zero-padding the sequence number keeps keys lexicographically ordered.
func chunkKey(sessionID string, seq uint64) []byte {
	return []byte(fmt.Sprintf("%s/%016x", sessionID, seq))
}

// SaveChunk persists a single PTY output chunk for the given session.
func (s *BoltSessionStore) SaveChunk(sessionID string, chunk OutputChunk) error {
	data, err := json.Marshal(chunk)
	if err != nil {
		return fmt.Errorf("marshal chunk seq=%d: %w", chunk.Seq, err)
	}
	return s.db.Batch(func(tx *bolt.Tx) error {
		return tx.Bucket(chunksBucket).Put(chunkKey(sessionID, chunk.Seq), data)
	})
}

// LoadChunks returns all persisted chunks for sessionID in ascending seq order.
func (s *BoltSessionStore) LoadChunks(sessionID string) ([]OutputChunk, error) {
	prefix := []byte(sessionID + "/")
	var chunks []OutputChunk
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(chunksBucket).Cursor()
		for k, v := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
			var chunk OutputChunk
			if err := json.Unmarshal(v, &chunk); err != nil {
				return fmt.Errorf("unmarshal chunk key=%q: %w", k, err)
			}
			chunks = append(chunks, chunk)
		}
		return nil
	})
	return chunks, err
}

// LoadAll returns every session record currently in the store.
func (s *BoltSessionStore) LoadAll() ([]SessionInfo, error) {
	var infos []SessionInfo
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(sessionsBucket).ForEach(func(k, v []byte) error {
			var info SessionInfo
			if err := json.Unmarshal(v, &info); err != nil {
				return fmt.Errorf("unmarshal session %q: %w", k, err)
			}
			infos = append(infos, info)
			return nil
		})
	})
	return infos, err
}

// Close closes the underlying database.
func (s *BoltSessionStore) Close() error {
	return s.db.Close()
}
