package telemetry_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	bridgev1 "github.com/orchael/bridgectl/gen/bridge/v1"
	"github.com/orchael/bridgectl/internal/telemetry"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// TestTelemetryS3FixturesMeetCollectorContract covers TEL-113 without AWS: the
// same fixture used by the opt-in S3 test must pass the real collector boundary.
func TestTelemetryS3FixturesMeetCollectorContract(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	spool, err := telemetry.NewSegmentSpool(t.TempDir(), 64<<10, 2<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	bridgev1.RegisterTelemetryCollectorServiceServer(server, telemetry.NewGRPCCollectorServer(spool, 64<<10, telemetry.EventQuestion, telemetry.EventAnswer))
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Stop(); _ = listener.Close() })
	conn, err := grpc.NewClient("passthrough:///s3-fixture", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
		return listener.Dial()
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	stream, err := bridgev1.NewTelemetryCollectorServiceClient(conn).StreamSegments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var payload bytes.Buffer
	for _, event := range s3FixtureEvents(time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)) {
		if err := json.NewEncoder(&payload).Encode(event); err != nil {
			t.Fatal(err)
		}
	}
	segmentID := "s3-fixture-contract"
	if err := stream.Send(&bridgev1.TelemetrySegment{Id: segmentID, Jsonl: payload.Bytes()}); err != nil {
		t.Fatal(err)
	}
	ack, err := stream.Recv()
	if err != nil {
		t.Fatalf("S3 fixture rejected by collector: %v", err)
	}
	if ack.GetId() != segmentID || !ack.GetStored() {
		t.Fatalf("unexpected fixture acknowledgement: %v", ack)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
}

func s3FixtureEvents(now time.Time) []telemetry.Event {
	return []telemetry.Event{
		{SchemaVersion: 2, Timestamp: now, SourceID: "s3-e2e-source", SessionID: "s3-e2e", Sequence: 1, Provider: "codex", Kind: telemetry.EventQuestion, Class: telemetry.ClassPermission, Fingerprint: "e2e-question", Text: "Proceed?"},
		{SchemaVersion: 2, Timestamp: now.Add(time.Second), SourceID: "s3-e2e-source", SessionID: "s3-e2e", Sequence: 2, Provider: "codex", Kind: telemetry.EventAnswer, Fingerprint: "e2e-question", Decision: telemetry.DecisionAccepted, LatencyMS: 1000},
	}
}

// TestTelemetryGRPCToS3 proves TEL-113 against an operator-created bucket. It
// never creates or deletes the bucket and removes only its generated prefix.
func TestTelemetryGRPCToS3(t *testing.T) {
	bucket := os.Getenv("BRIDGECTL_TELEMETRY_S3_BUCKET")
	if bucket == "" {
		t.Skip("set BRIDGECTL_TELEMETRY_S3_BUCKET to run the real-S3 telemetry E2E")
	}
	basePrefix := strings.Trim(os.Getenv("BRIDGECTL_TELEMETRY_S3_PREFIX"), "/")
	if basePrefix == "" {
		basePrefix = "bridgectl/telemetry"
	}
	testPrefix := path.Join(basePrefix, "e2e", uuid.NewString())

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	client := s3.NewFromConfig(awsCfg)
	t.Cleanup(func() { cleanupS3Prefix(t, client, bucket, testPrefix) })

	collectorSpool, err := telemetry.NewSegmentSpool(t.TempDir(), 64<<10, 2<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	grpcServer := grpc.NewServer()
	bridgev1.RegisterTelemetryCollectorServiceServer(grpcServer, telemetry.NewGRPCCollectorServer(collectorSpool, 64<<10, telemetry.EventQuestion, telemetry.EventAnswer))
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(func() {
		grpcServer.Stop()
		_ = listener.Close()
	})

	objectStore, err := telemetry.NewS3ObjectStore(ctx, awsCfg.Region, "", false)
	if err != nil {
		t.Fatal(err)
	}
	uploadCtx, stopUpload := context.WithCancel(context.Background())
	uploader := telemetry.NewSegmentUploader(collectorSpool, objectStore, bucket, testPrefix)
	uploadDone := make(chan struct{})
	go func() {
		defer close(uploadDone)
		uploader.Run(uploadCtx, 25*time.Millisecond, func(err error) { t.Logf("S3 upload retry: %v", err) })
	}()
	defer func() {
		stopUpload()
		<-uploadDone
	}()

	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	bridgeSpool, err := telemetry.NewSegmentSpool(t.TempDir(), 64<<10, 2<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	forwarder := telemetry.NewGRPCForwardingSink(bridgeSpool, bridgev1.NewTelemetryCollectorServiceClient(conn), conn, 25*time.Millisecond, 25*time.Millisecond, func(err error) {
		t.Logf("gRPC delivery retry: %v", err)
	})
	now := time.Now().UTC()
	for _, event := range s3FixtureEvents(now) {
		if err := forwarder.Record(event); err != nil {
			t.Fatal(err)
		}
	}
	flushCtx, flushCancel := context.WithTimeout(ctx, 15*time.Second)
	defer flushCancel()
	if err := forwarder.Close(flushCtx); err != nil {
		t.Fatal(err)
	}

	key := waitForS3Object(t, ctx, client, bucket, testPrefix)
	result, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(result.Body)
	closeErr := result.Body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read S3 object: read=%v close=%v", readErr, closeErr)
	}
	var kinds []telemetry.EventKind
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
		var event telemetry.Event
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatalf("decode S3 JSONL: %v\n%s", err, data)
		}
		if event.SchemaVersion != 2 || event.SourceID != "s3-e2e-source" || event.SessionID != "s3-e2e" || event.Sequence != uint64(len(kinds)+1) {
			t.Fatalf("invalid S3 event envelope: %+v", event)
		}
		kinds = append(kinds, event.Kind)
	}
	if len(kinds) != 2 || kinds[0] != telemetry.EventQuestion || kinds[1] != telemetry.EventAnswer {
		t.Fatalf("S3 event kinds=%v, want question/answer; object=%s", kinds, key)
	}
	t.Logf("verified bridge -> gRPC collector -> s3://%s/%s", bucket, key)
}

func waitForS3Object(t *testing.T, ctx context.Context, client *s3.Client, bucket, prefix string) string {
	t.Helper()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		result, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix + "/")})
		if err == nil && len(result.Contents) > 0 {
			return aws.ToString(result.Contents[0].Key)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for S3 object under s3://%s/%s: %v (last list error: %v)", bucket, prefix, ctx.Err(), err)
		case <-ticker.C:
		}
	}
}

func cleanupS3Prefix(t *testing.T, client *s3.Client, bucket, prefix string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix + "/")})
	if err != nil {
		t.Errorf("list S3 E2E cleanup prefix: %v", err)
		return
	}
	for _, object := range result.Contents {
		if !strings.HasPrefix(aws.ToString(object.Key), prefix+"/") {
			t.Errorf("refusing to delete object outside E2E prefix: %q", aws.ToString(object.Key))
			continue
		}
		if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: object.Key}); err != nil {
			t.Errorf("delete S3 E2E object %q: %v", aws.ToString(object.Key), err)
		}
	}
	if len(result.Contents) == 0 {
		t.Logf("no S3 E2E objects remained under %s", prefix)
	}
}
