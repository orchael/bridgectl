package telemetry

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestSegmentUploaderRetainsUntilSuccessfulPut proves TEL-112: an S3-style
// object failure cannot delete the collector's durable local segment.
func TestSegmentUploaderRetainsUntilSuccessfulPut(t *testing.T) {
	spool, err := NewSegmentSpool(t.TempDir(), 1<<20, 10<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("{\"kind\":\"question\"}\n")
	if _, err := spool.Accept("20260914T120000.000000000Z-batch-1", payload); err != nil {
		t.Fatal(err)
	}
	objects := &fakeObjectStore{err: errors.New("S3 unavailable")}
	uploader := NewSegmentUploader(spool, objects, "existing-bucket", "bridge/telemetry")
	if err := uploader.UploadPending(context.Background()); err == nil {
		t.Fatal("UploadPending succeeded while object store was unavailable")
	}
	segments, err := spool.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 1 {
		t.Fatalf("segments after failure=%v, want retained segment", segments)
	}

	objects.err = nil
	if err := uploader.UploadPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	segments, err = spool.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 0 {
		t.Fatalf("segments after upload=%v, want empty spool", segments)
	}
	if objects.bucket != "existing-bucket" || !strings.HasSuffix(objects.key, "/20260914T120000.000000000Z-batch-1.jsonl") || !strings.HasPrefix(objects.key, "bridge/telemetry/2026/09/14/") {
		t.Fatalf("uploaded bucket/key=%q/%q", objects.bucket, objects.key)
	}
	if string(objects.data) != string(payload) {
		t.Fatalf("uploaded data=%q, want %q", objects.data, payload)
	}
}

type fakeObjectStore struct {
	bucket string
	key    string
	data   []byte
	err    error
}

func (s *fakeObjectStore) Put(_ context.Context, bucket, key string, data []byte) error {
	s.bucket = bucket
	s.key = key
	s.data = append([]byte(nil), data...)
	return s.err
}
