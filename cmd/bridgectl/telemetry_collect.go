package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	bridgev1 "github.com/orchael/bridgectl/gen/bridge/v1"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/orchael/bridgectl/internal/config"
	"github.com/orchael/bridgectl/internal/localserver"
	"github.com/orchael/bridgectl/internal/telemetry"
)

func newTelemetryCollectCmd() *cobra.Command {
	var listenAddress string
	var spoolDir string
	var kinds []string
	var maxSegmentBytes int64
	var maxDiskSpace string
	var tlsCert string
	var tlsKey string
	var s3Bucket string
	var s3Prefix string
	var s3Region string
	var s3Endpoint string
	var s3ForcePathStyle bool
	var s3UploadInterval time.Duration
	cmd := &cobra.Command{
		Use:   "collect",
		Short: "Run a private-network telemetry collector",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			eventKinds, err := parseTelemetryEventKinds(kinds)
			if err != nil {
				return err
			}
			maxDiskBytes, err := config.ParseByteSize(maxDiskSpace)
			if err != nil {
				return fmt.Errorf("collector --max-disk-space: %w", err)
			}
			if maxSegmentBytes < 1 || maxSegmentBytes > math.MaxInt32 || maxDiskBytes < maxSegmentBytes {
				return fmt.Errorf("collector segment size must be positive and gRPC-safe, and --max-disk-space must be at least the segment size")
			}
			if (tlsCert == "") != (tlsKey == "") {
				return fmt.Errorf("collector --tls-cert and --tls-key must be set together")
			}
			if s3Bucket != "" && s3UploadInterval <= 0 {
				return fmt.Errorf("collector --s3-upload-interval must be positive")
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return runTelemetryCollector(ctx, listenAddress, spoolDir, maxSegmentBytes, maxDiskBytes, eventKinds, tlsCert, tlsKey, telemetryS3Options{
				bucket: s3Bucket, prefix: s3Prefix, region: s3Region, endpoint: s3Endpoint,
				forcePathStyle: s3ForcePathStyle, uploadInterval: s3UploadInterval,
			})
		},
	}
	cmd.Flags().StringVar(&listenAddress, "listen", "127.0.0.1:9464", "collector gRPC listen address")
	cmd.Flags().StringVar(&spoolDir, "spool-dir", localserver.TelemetrySpoolDir("", localserver.StateDir()), "durable segmented JSONL spool directory")
	cmd.Flags().StringSliceVar(&kinds, "kinds", nil, "event kinds to retain (default: all)")
	cmd.Flags().Int64Var(&maxSegmentBytes, "max-segment-bytes", 10<<20, "maximum bytes accepted in one immutable segment")
	cmd.Flags().StringVar(&maxDiskSpace, "max-disk-space", "1GB", "maximum local spool disk usage before oldest-first eviction (for example 500MB or 1GiB)")
	cmd.Flags().StringVar(&tlsCert, "tls-cert", "", "PEM server certificate for TLS (requires --tls-key)")
	cmd.Flags().StringVar(&tlsKey, "tls-key", "", "PEM server private key for TLS (requires --tls-cert)")
	cmd.Flags().StringVar(&s3Bucket, "s3-bucket", "", "existing S3 bucket for uploaded segments (empty keeps segments on the volume)")
	cmd.Flags().StringVar(&s3Prefix, "s3-prefix", "bridgectl/telemetry", "S3 key prefix")
	cmd.Flags().StringVar(&s3Region, "s3-region", "", "AWS region override (default: standard AWS configuration)")
	cmd.Flags().StringVar(&s3Endpoint, "s3-endpoint", "", "optional S3-compatible endpoint")
	cmd.Flags().BoolVar(&s3ForcePathStyle, "s3-force-path-style", false, "use path-style S3 addressing")
	cmd.Flags().DurationVar(&s3UploadInterval, "s3-upload-interval", time.Second, "interval between pending segment uploads")
	return cmd
}

type telemetryS3Options struct {
	bucket         string
	prefix         string
	region         string
	endpoint       string
	forcePathStyle bool
	uploadInterval time.Duration
}

func parseTelemetryEventKinds(values []string) ([]telemetry.EventKind, error) {
	if len(values) == 1 && values[0] == "all" {
		return nil, nil
	}
	for _, value := range values {
		if value == "all" {
			return nil, fmt.Errorf("telemetry event kind %q cannot be combined with concrete kinds", value)
		}
	}
	seen := make(map[telemetry.EventKind]bool, len(values))
	kinds := make([]telemetry.EventKind, 0, len(values))
	for _, value := range values {
		kind := telemetry.EventKind(value)
		switch kind {
		case telemetry.EventSessionStarted, telemetry.EventProviderOutput, telemetry.EventUserInput, telemetry.EventQuestion, telemetry.EventAnswer, telemetry.EventSessionEnded:
		default:
			return nil, fmt.Errorf("unsupported telemetry event kind %q", value)
		}
		if seen[kind] {
			return nil, fmt.Errorf("duplicate telemetry event kind %q", value)
		}
		seen[kind] = true
		kinds = append(kinds, kind)
	}
	return kinds, nil
}

func runTelemetryCollector(ctx context.Context, listenAddress, spoolDir string, maxSegmentBytes, maxDiskBytes int64, kinds []telemetry.EventKind, tlsCert, tlsKey string, s3Options telemetryS3Options) error {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)
	spool, err := telemetry.NewSegmentSpool(spoolDir, maxSegmentBytes, maxDiskBytes, func(segment telemetry.Segment) {
		logger.Warn("telemetry collector spool evicted oldest segment", "segment_id", segment.ID, "bytes", segment.Size)
	})
	if err != nil {
		return fmt.Errorf("open telemetry collector spool: %w", err)
	}
	var uploader *telemetry.SegmentUploader
	if s3Options.bucket != "" {
		objectStore, err := telemetry.NewS3ObjectStore(ctx, s3Options.region, s3Options.endpoint, s3Options.forcePathStyle)
		if err != nil {
			return fmt.Errorf("configure telemetry S3 storage: %w", err)
		}
		uploader = telemetry.NewSegmentUploader(spool, objectStore, s3Options.bucket, s3Options.prefix)
		uploadCtx, stopUploader := context.WithCancel(ctx)
		uploadDone := make(chan struct{})
		go func() {
			defer close(uploadDone)
			uploader.Run(uploadCtx, s3Options.uploadInterval, func(err error) {
				logger.Warn("telemetry S3 upload failed; segment retained", "error", err)
			})
		}()
		defer func() {
			stopUploader()
			<-uploadDone
			flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := uploader.UploadPending(flushCtx); err != nil {
				logger.Warn("telemetry S3 shutdown flush failed; segment retained", "error", err)
			}
		}()
	}
	listener, err := net.Listen("tcp", listenAddress)
	if err != nil {
		return fmt.Errorf("listen for telemetry: %w", err)
	}
	defer func() { _ = listener.Close() }()
	serverOptions := []grpc.ServerOption{grpc.MaxRecvMsgSize(int(maxSegmentBytes) + 1024)}
	if tlsCert != "" {
		certificate, err := tls.LoadX509KeyPair(tlsCert, tlsKey)
		if err != nil {
			return fmt.Errorf("load telemetry collector TLS identity: %w", err)
		}
		serverOptions = append(serverOptions, grpc.Creds(credentials.NewTLS(&tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{certificate},
		})))
	}
	server := grpc.NewServer(serverOptions...)
	bridgev1.RegisterTelemetryCollectorServiceServer(server, telemetry.NewGRPCCollectorServer(spool, maxSegmentBytes, kinds...))
	healthServer := health.NewServer()
	healthv1.RegisterHealthServer(server, healthServer)
	healthServer.SetServingStatus("", healthv1.HealthCheckResponse_SERVING)
	logger.Info("telemetry collector listening", "version", version, "address", listener.Addr().String(), "spool_dir", spoolDir, "max_segment_bytes", maxSegmentBytes, "max_disk_bytes", maxDiskBytes, "kinds", kinds, "tls", tlsCert != "", "s3", s3Options.bucket != "")
	errCh := make(chan error, 1)
	go func() { errCh <- server.Serve(listener) }()

	select {
	case err := <-errCh:
		if errors.Is(err, grpc.ErrServerStopped) {
			return nil
		}
		return fmt.Errorf("serve telemetry: %w", err)
	case <-ctx.Done():
		healthServer.SetServingStatus("", healthv1.HealthCheckResponse_NOT_SERVING)
		stopped := make(chan struct{})
		go func() {
			server.GracefulStop()
			close(stopped)
		}()
		select {
		case <-stopped:
			return nil
		case <-time.After(5 * time.Second):
			server.Stop()
			<-stopped
			return nil
		}
	}
}
