package telemetry

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestS3ObjectStorePut(t *testing.T) {
	api := &fakeS3PutAPI{}
	store := &S3ObjectStore{client: api}
	if err := store.Put(context.Background(), "bucket", "prefix/segment.jsonl", []byte("event\n")); err != nil {
		t.Fatal(err)
	}
	if aws.ToString(api.input.Bucket) != "bucket" || aws.ToString(api.input.Key) != "prefix/segment.jsonl" || aws.ToString(api.input.ContentType) != "application/x-ndjson" {
		t.Fatalf("PutObject input=%+v", api.input)
	}
	data, err := io.ReadAll(api.input.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "event\n" {
		t.Fatalf("body=%q", data)
	}

	api.err = errors.New("access denied")
	if err := store.Put(context.Background(), "bucket", "key", nil); err == nil {
		t.Fatal("Put succeeded despite S3 error")
	}
}

type fakeS3PutAPI struct {
	input *s3.PutObjectInput
	err   error
}

func (f *fakeS3PutAPI) PutObject(_ context.Context, input *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.input = input
	return &s3.PutObjectOutput{}, f.err
}
