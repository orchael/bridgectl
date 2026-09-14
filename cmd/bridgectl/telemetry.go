package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/orchael/bridgectl/internal/config"
	"github.com/orchael/bridgectl/internal/localserver"
	"github.com/orchael/bridgectl/internal/telemetry"
)

const defaultTelemetryWindow = 7 * 24 * time.Hour

func newTelemetryCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "telemetry",
		Short: "Inspect local question-friction telemetry",
	}
	cmd.PersistentFlags().StringVar(&configPath, "config", "", "path to bridge YAML config (default: discovered server config)")
	cmd.AddCommand(newTelemetryReportCmd(&configPath), newTelemetryExportCmd(&configPath), newTelemetryCollectCmd(), newTelemetryHealthCmd())
	return cmd
}

func newTelemetryHealthCmd() *cobra.Command {
	var target string
	var allowInsecure bool
	var caPath string
	var serverName string
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "health",
		Short: "Check a telemetry collector's gRPC health",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if allowInsecure && (caPath != "" || serverName != "") {
				return fmt.Errorf("--ca and --server-name require TLS")
			}
			var transportCredentials credentials.TransportCredentials
			if allowInsecure {
				transportCredentials = insecure.NewCredentials()
			} else {
				tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName}
				if caPath != "" {
					caPEM, err := os.ReadFile(filepath.Clean(caPath))
					if err != nil {
						return fmt.Errorf("read collector CA: %w", err)
					}
					roots, err := x509.SystemCertPool()
					if err != nil {
						roots = x509.NewCertPool()
					}
					if !roots.AppendCertsFromPEM(caPEM) {
						return fmt.Errorf("parse collector CA: no certificates found")
					}
					tlsConfig.RootCAs = roots
				}
				transportCredentials = credentials.NewTLS(tlsConfig)
			}
			conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(transportCredentials))
			if err != nil {
				return err
			}
			defer func() { _ = conn.Close() }()
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			response, err := healthv1.NewHealthClient(conn).Check(ctx, &healthv1.HealthCheckRequest{})
			if err != nil {
				return fmt.Errorf("check telemetry collector health: %w", err)
			}
			if response.GetStatus() != healthv1.HealthCheckResponse_SERVING {
				return fmt.Errorf("telemetry collector is %s", response.GetStatus())
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), "SERVING")
			return err
		},
	}
	cmd.Flags().StringVar(&target, "target", "127.0.0.1:9464", "collector gRPC target")
	cmd.Flags().BoolVar(&allowInsecure, "insecure", false, "allow plaintext gRPC (private local networks only)")
	cmd.Flags().StringVar(&caPath, "ca", "", "PEM CA bundle for collector TLS")
	cmd.Flags().StringVar(&serverName, "server-name", "", "collector TLS server name override")
	cmd.Flags().DurationVar(&timeout, "timeout", 3*time.Second, "health check timeout")
	return cmd
}

func newTelemetryReportCmd(configPath *string) *cobra.Command {
	var eventsPath string
	var since time.Duration
	var top int
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Summarize recent question-friction events",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			defaults, err := loadTelemetryCommandDefaults(*configPath)
			if err != nil {
				return err
			}
			if !cmd.Flags().Changed("events") {
				eventsPath = defaults.eventsPath
			}
			if !cmd.Flags().Changed("since") {
				since = defaults.window
			}
			if since <= 0 {
				return fmt.Errorf("--since must be positive")
			}
			if top < 0 {
				return fmt.Errorf("--top must not be negative")
			}
			until := time.Now().UTC()
			report, err := telemetry.BuildReportFromPath(eventsPath, telemetry.ReportOptions{
				Since: until.Add(-since),
				Until: until,
				Top:   top,
			})
			if err != nil {
				return err
			}
			return writeTelemetryReport(cmd.OutOrStdout(), report)
		},
	}
	cmd.Flags().StringVar(&eventsPath, "events", "", "path to the telemetry JSONL event file")
	cmd.Flags().DurationVar(&since, "since", 0, "rolling window to report (default: configured rolling_window)")
	cmd.Flags().IntVar(&top, "top", 10, "number of recurring question fingerprints to show")
	return cmd
}

func newTelemetryExportCmd(configPath *string) *cobra.Command {
	var eventsPath string
	var since time.Duration
	var format string
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export provider-neutral telemetry feedback",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			defaults, err := loadTelemetryCommandDefaults(*configPath)
			if err != nil {
				return err
			}
			if !cmd.Flags().Changed("events") {
				eventsPath = defaults.eventsPath
			}
			if !cmd.Flags().Changed("since") {
				since = defaults.window
			}
			if format != "json" {
				return fmt.Errorf("unsupported telemetry export format %q (want json)", format)
			}
			if since <= 0 {
				return fmt.Errorf("--since must be positive")
			}
			until := time.Now().UTC()
			feedback, err := telemetry.BuildFeedbackFromPath(eventsPath, until.Add(-since), until)
			if err != nil {
				return err
			}
			encoder := json.NewEncoder(cmd.OutOrStdout())
			encoder.SetIndent("", "  ")
			if err := encoder.Encode(feedback); err != nil {
				return fmt.Errorf("encode telemetry feedback: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&eventsPath, "events", "", "path to the telemetry JSONL event file")
	cmd.Flags().DurationVar(&since, "since", 0, "rolling window to export (default: configured rolling_window)")
	cmd.Flags().StringVar(&format, "format", "json", "export format (json)")
	return cmd
}

type telemetryCommandDefaults struct {
	eventsPath string
	window     time.Duration
}

func loadTelemetryCommandDefaults(configPath string) (telemetryCommandDefaults, error) {
	stateDir := localserver.StateDir()
	defaults := telemetryCommandDefaults{
		eventsPath: localserver.TelemetrySpoolDir("", stateDir),
		window:     defaultTelemetryWindow,
	}
	if configPath == "" {
		configPath = defaultServerConfigPath(stateDir)
		if configPath == "" {
			return defaults, nil
		}
	}
	cfg, err := config.Load(filepath.Clean(configPath))
	if err != nil {
		return telemetryCommandDefaults{}, fmt.Errorf("load telemetry config %q: %w", configPath, err)
	}
	window, err := time.ParseDuration(cfg.Telemetry.RollingWindow)
	if err != nil {
		return telemetryCommandDefaults{}, fmt.Errorf("parse telemetry rolling window: %w", err)
	}
	defaults.eventsPath = localserver.TelemetrySpoolDir(cfg.Telemetry.SpoolDir, stateDir)
	defaults.window = window
	return defaults, nil
}

func writeTelemetryReport(out io.Writer, report telemetry.Report) error {
	lines := []string{
		fmt.Sprintf("Window:                 %s to %s", report.Since.Format(time.RFC3339), report.Until.Format(time.RFC3339)),
		fmt.Sprintf("Sessions:               %d", report.Sessions),
		fmt.Sprintf("Agent hours:            %.2f", report.AgentHours),
		fmt.Sprintf("Questions:              %d", report.Questions),
		fmt.Sprintf("Questions/session:      %.2f", report.QuestionsPerSession),
		fmt.Sprintf("Questions/agent-hour:   %.2f", report.QuestionsPerAgentHour),
		fmt.Sprintf("Acceptance rate:        %.1f%%", report.AcceptanceRate*100),
		fmt.Sprintf("Rejection rate:         %.1f%%", report.RejectionRate*100),
		fmt.Sprintf("Changed-answer rate:    %.1f%%", report.ChangeRate*100),
		fmt.Sprintf("Unknown-answer rate:    %.1f%%", report.UnknownRate*100),
		fmt.Sprintf("Median answer latency:  %s", time.Duration(report.MedianLatencyMS)*time.Millisecond),
		"Top fingerprints:",
	}
	for _, question := range report.TopQuestions {
		lines = append(lines, fmt.Sprintf("  %s  class=%s asked=%d accepted=%d rejected=%d changed=%d unknown=%d",
			question.Fingerprint, question.Class, question.Asked, question.Accepted,
			question.Rejected, question.Changed, question.Unknown))
	}
	if _, err := io.WriteString(out, strings.Join(lines, "\n")+"\n"); err != nil {
		return fmt.Errorf("write telemetry report: %w", err)
	}
	return nil
}
