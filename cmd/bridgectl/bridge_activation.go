package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/orchael/bridgectl/internal/bridgecontrol"
	"github.com/orchael/bridgectl/internal/localserver"
	"github.com/spf13/cobra"
)

func activateBridgeEnrollment(parent context.Context, cmd *cobra.Command) error {
	if err := ensureServer(); err != nil {
		return fmt.Errorf("enrollment saved, but could not start Bridge reporting: %w", err)
	}
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	if err := localserver.ReloadEnrollment(ctx, localserver.StateDir(), bridgeConfigPath()); err != nil {
		return fmt.Errorf("enrollment saved, but activation failed: %w", err)
	}
	enrollment, err := readEnrollmentMetadata()
	if err != nil {
		return err
	}
	if enrollment.ControlEndpoint == "" {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "✓ Reporting configuration activated (Bridge has not provisioned control)")
		return nil
	}
	if err := waitForBridgeControl(ctx, localserver.StateDir(), enrollment.InstallationID); err != nil {
		return fmt.Errorf("enrollment saved, but control is not connected; run bridgectl doctor: %w", err)
	}
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), "✓ Reporting configuration activated\n✓ Bridge control connected")
	return nil
}

func waitForBridgeControl(ctx context.Context, stateDir, installationID string) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var state bridgecontrol.State
	for {
		if st, err := bridgecontrol.ReadStatus(filepath.Join(stateDir, "bridge-control-status.json")); err == nil {
			state = st.State
			if st.State == bridgecontrol.StateConnected && st.InstallationID == installationID && time.Since(st.UpdatedAt) < bridgecontrol.StatusStaleAfter {
				return nil
			}
			if st.State == bridgecontrol.StateAuthRejected {
				return errors.New("bridge rejected the control credential")
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("control state %q: %w", state, ctx.Err())
		case <-ticker.C:
		}
	}
}
