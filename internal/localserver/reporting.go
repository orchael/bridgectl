package localserver

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"

	bridgev1 "github.com/orchael/bridgectl/gen/bridge/v1"
	"github.com/orchael/bridgectl/internal/bridge"
	"github.com/orchael/bridgectl/internal/bridgecontrol"
	"github.com/orchael/bridgectl/internal/config"
	"github.com/orchael/bridgectl/internal/telemetry"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

func newTelemetryObserver(telemetryCfg config.TelemetryConfig, stateDir, version string, logger *slog.Logger) (bridge.TelemetryObserver, error) {
	if telemetryCfg.Enabled {
		spoolDir := TelemetrySpoolDir(telemetryCfg.SpoolDir, stateDir)
		sourceID, err := telemetry.ResolveSourceID(telemetryCfg.SourceID, filepath.Join(stateDir, "telemetry", "source-id"))
		if err != nil {
			return nil, fmt.Errorf("configure telemetry source identity: %w", err)
		}
		identityKeyPath := filepath.Join(stateDir, "telemetry", "identity-key")
		if telemetryCfg.IdentityKeyFile != "" {
			identityKeyPath = expandTelemetryPath(telemetryCfg.IdentityKeyFile)
		}
		identityKey, err := telemetry.LoadOrCreateIdentityKey(identityKeyPath)
		if err != nil {
			return nil, fmt.Errorf("configure telemetry context identity: %w", err)
		}
		maxDiskBytes, err := config.ParseByteSize(telemetryCfg.MaxDiskSpace)
		if err != nil {
			return nil, fmt.Errorf("configure telemetry disk budget: %w", err)
		}
		segmentSpool, err := telemetry.NewSegmentSpool(spoolDir, telemetryCfg.MaxSegmentBytes, maxDiskBytes, func(segment telemetry.Segment) {
			logger.Warn("telemetry spool evicted oldest segment", "segment_id", segment.ID, "bytes", segment.Size)
		})
		if err != nil {
			return nil, fmt.Errorf("configure telemetry spool: %w", err)
		}
		var eventSink telemetry.Sink
		destination := "local"
		if telemetryCfg.CollectorURL != "" && telemetryCfg.CollectorTarget == "" {
			credentialPath := telemetryCfg.CollectorCredentialFile
			if credentialPath == "" {
				credentialPath = filepath.Join(stateDir, "bridge-credentials.json")
			}
			credentialData, readErr := SecureReadFile(expandTelemetryPath(credentialPath))
			if readErr != nil {
				return nil, fmt.Errorf("read telemetry collector credential: %w", readErr)
			}
			var credential struct {
				CollectorCredential string `json:"collector_credential"`
			}
			if readErr = json.Unmarshal(credentialData, &credential); readErr != nil || !CollectorCredentialPattern.MatchString(credential.CollectorCredential) {
				return nil, fmt.Errorf("invalid telemetry collector credential")
			}
			eventSink = telemetry.NewHTTPForwardingSink(segmentSpool, telemetryCfg.CollectorURL, credential.CollectorCredential, version, config.ParseDuration(telemetryCfg.FlushInterval, telemetry.DefaultFlushInterval), config.ParseDuration(telemetryCfg.RetryInterval, time.Second), func(err error) { logger.Warn("telemetry delivery", "error", err) })
			destination = "https_collector"
		} else if telemetryCfg.CollectorTarget != "" {
			var transportCredentials credentials.TransportCredentials
			if telemetryCfg.CollectorInsecure {
				transportCredentials = insecure.NewCredentials()
			} else {
				host, _, _ := net.SplitHostPort(telemetryCfg.CollectorTarget)
				serverName := telemetryCfg.CollectorServerName
				if serverName == "" {
					serverName = host
				}
				tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName}
				if telemetryCfg.CollectorCA != "" {
					caPEM, readErr := os.ReadFile(expandTelemetryPath(telemetryCfg.CollectorCA))
					if readErr != nil {
						return nil, fmt.Errorf("read telemetry collector CA: %w", readErr)
					}
					roots, poolErr := x509.SystemCertPool()
					if poolErr != nil {
						roots = x509.NewCertPool()
					}
					if !roots.AppendCertsFromPEM(caPEM) {
						return nil, fmt.Errorf("parse telemetry collector CA: no certificates found")
					}
					tlsConfig.RootCAs = roots
				}
				transportCredentials = credentials.NewTLS(tlsConfig)
			}
			conn, err := grpc.NewClient(telemetryCfg.CollectorTarget, grpc.WithTransportCredentials(transportCredentials))
			if err != nil {
				return nil, fmt.Errorf("configure telemetry collector: %w", err)
			}
			eventSink = telemetry.NewGRPCForwardingSink(
				segmentSpool,
				bridgev1.NewTelemetryCollectorServiceClient(conn),
				conn,
				config.ParseDuration(telemetryCfg.FlushInterval, telemetry.DefaultFlushInterval),
				config.ParseDuration(telemetryCfg.RetryInterval, time.Second),
				func(err error) { logger.Warn("telemetry delivery", "error", err) },
			)
			destination = "grpc_collector"
		} else {
			eventSink = telemetry.NewLocalSpoolingSink(segmentSpool,
				config.ParseDuration(telemetryCfg.FlushInterval, telemetry.DefaultFlushInterval),
				func(err error) { logger.Warn("telemetry local flush", "error", err) },
			)
		}
		kinds := make([]telemetry.EventKind, 0, len(telemetryCfg.Kinds))
		for _, kind := range telemetryCfg.Kinds {
			kinds = append(kinds, telemetry.EventKind(kind))
		}
		collector := telemetry.NewLiveCollectorWithIdentity(
			eventSink,
			telemetryCfg.QueueSize,
			telemetryCfg.IncludeRedactedText,
			telemetry.LiveIdentity{SourceID: sourceID, ActorID: telemetryCfg.ActorID, SourceLabel: telemetryCfg.SourceLabel, ContextKey: identityKey},
			func(err error) { logger.Warn("telemetry persistence", "error", err) },
			kinds...,
		)
		logger.Info("telemetry enabled", "source_id", sourceID, "destination", destination, "kinds", telemetryCfg.Kinds, "rolling_window", telemetryCfg.RollingWindow)
		return collector, nil
	}
	return nil, nil
}

func newControlClient(controlCfg config.ControlConfig, stateDir, version string, logger *slog.Logger, sup *bridge.Supervisor) (*bridgecontrol.Client, error) {
	if controlCfg.Endpoint == "" || controlCfg.CredentialFile == "" {
		return nil, nil
	}
	if err := bridgecontrol.ValidateEndpoint(controlCfg.Endpoint); err != nil {
		return nil, fmt.Errorf("invalid control endpoint: %w", err)
	}
	data, err := SecureReadFile(expandTelemetryPath(controlCfg.CredentialFile))
	if err != nil {
		return nil, fmt.Errorf("read control credential: %w", err)
	}
	var credential struct {
		ControlCredential string `json:"control_credential"`
	}
	if json.Unmarshal(data, &credential) != nil || !ControlCredentialPattern.MatchString(credential.ControlCredential) {
		return nil, fmt.Errorf("invalid or missing control credential")
	}
	return bridgecontrol.New(bridgecontrol.Config{
		TerminalSupervisor: func() *bridge.Supervisor { return sup },
		CommandPath:        filepath.Join(stateDir, "bridge-control-commands"),
		ObserveFunc: func(id string, after uint64, events, bytes int) (bridge.ActivityWindow, error) {
			if sup == nil {
				return bridge.ActivityWindow{}, bridge.ErrSessionNotFound
			}
			return sup.ObserveActivity(id, after, events, bytes)
		},
		InstructionFunc: func(ctx context.Context, id, _ string, text string) error {
			if sup == nil {
				return bridge.ErrSessionNotFound
			}
			return sup.SendInstruction(ctx, id, text)
		},
		ApprovalFunc: func(ctx context.Context, sessionID, pendingID, decision string) error {
			if sup == nil {
				return bridge.ErrSessionNotFound
			}
			return sup.DecideApproval(ctx, sessionID, pendingID, decision)
		},
		RespondFunc: func(ctx context.Context, sessionID, pendingID, text string) error {
			if sup == nil {
				return bridge.ErrSessionNotFound
			}
			return sup.RespondToInput(ctx, sessionID, pendingID, text)
		},
		Endpoint:         controlCfg.Endpoint,
		Credential:       credential.ControlCredential,
		BridgectlVersion: version,
		StatusPath:       filepath.Join(stateDir, "bridge-control-status.json"),
		RevisionPath:     filepath.Join(stateDir, "bridge-control-revisions.json"),
		Logger:           logger,
		SnapshotFunc: func() []bridgecontrol.SessionSnapshot {
			if sup == nil {
				return nil
			}
			return bridgecontrol.ActiveSnapshots(sup.List(""))
		},
	}), nil
}
