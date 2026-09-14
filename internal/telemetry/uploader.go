package telemetry

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"
)

// ObjectStore is the immutable object boundary used by the collector uploader.
type ObjectStore interface {
	Put(context.Context, string, string, []byte) error
}

// SegmentUploader copies collector segments to immutable object storage and
// removes local copies only after Put returns success.
type SegmentUploader struct {
	spool  *SegmentSpool
	store  ObjectStore
	bucket string
	prefix string
}

func NewSegmentUploader(spool *SegmentSpool, store ObjectStore, bucket, prefix string) *SegmentUploader {
	return &SegmentUploader{spool: spool, store: store, bucket: bucket, prefix: strings.Trim(prefix, "/")}
}

func (u *SegmentUploader) UploadPending(ctx context.Context) error {
	segments, err := u.spool.Pending()
	if err != nil {
		return err
	}
	for _, segment := range segments {
		data, err := u.spool.Read(segment.ID)
		if err != nil {
			return err
		}
		key := u.objectKey(segment.ID)
		if err := u.store.Put(ctx, u.bucket, key, data); err != nil {
			return fmt.Errorf("put telemetry segment %s: %w", segment.ID, err)
		}
		if err := u.spool.Remove(segment.ID); err != nil {
			return fmt.Errorf("remove uploaded telemetry segment %s: %w", segment.ID, err)
		}
	}
	return nil
}

func (u *SegmentUploader) Run(ctx context.Context, interval time.Duration, onError func(error)) {
	if interval <= 0 {
		interval = time.Second
	}
	u.uploadAndReport(ctx, onError)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			u.uploadAndReport(ctx, onError)
		}
	}
}

func (u *SegmentUploader) uploadAndReport(ctx context.Context, onError func(error)) {
	if err := u.UploadPending(ctx); err != nil && onError != nil {
		onError(err)
	}
}

func (u *SegmentUploader) objectKey(id string) string {
	datePath := "undated"
	if len(id) >= 8 {
		if parsed, err := time.Parse("20060102", id[:8]); err == nil {
			datePath = parsed.UTC().Format("2006/01/02")
		}
	}
	return path.Join(u.prefix, datePath, id+".jsonl")
}
