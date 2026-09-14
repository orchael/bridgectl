package telemetry

import (
	"bytes"
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type s3PutObjectAPI interface {
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

// S3ObjectStore writes immutable telemetry segments through the standard AWS
// SDK credential and region provider chains.
type S3ObjectStore struct {
	client s3PutObjectAPI
}

func NewS3ObjectStore(ctx context.Context, region, endpoint string, forcePathStyle bool) (*S3ObjectStore, error) {
	loadOptions := make([]func(*awsconfig.LoadOptions) error, 0, 1)
	if region != "" {
		loadOptions = append(loadOptions, awsconfig.WithRegion(region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, loadOptions...)
	if err != nil {
		return nil, fmt.Errorf("load AWS configuration: %w", err)
	}
	client := s3.NewFromConfig(cfg, func(options *s3.Options) {
		options.UsePathStyle = forcePathStyle
		if endpoint != "" {
			options.BaseEndpoint = aws.String(endpoint)
		}
	})
	return &S3ObjectStore{client: client}, nil
}

func (s *S3ObjectStore) Put(ctx context.Context, bucket, key string, data []byte) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: aws.String("application/x-ndjson"),
	})
	if err != nil {
		return fmt.Errorf("put s3://%s/%s: %w", bucket, key, err)
	}
	return nil
}

var _ ObjectStore = (*S3ObjectStore)(nil)
