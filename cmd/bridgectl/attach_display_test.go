package main

import (
	"bytes"
	"strings"
	"testing"

	bridgev1 "github.com/orchael/bridgectl/gen/bridge/v1"
)

func TestAttachDisplay(t *testing.T) {
	gap := &bridgev1.AttachSessionEvent{Type: bridgev1.AttachEventType_ATTACH_EVENT_TYPE_REPLAY_GAP, OldestSeq: 81871, LastSeq: 81912}
	tests := []struct {
		name         string
		tty          bool
		wantOut      string
		wantImmed    string
		wantDeferred string
	}{
		{
			name:         "terminal resets and defers the notice",
			tty:          true,
			wantOut:      attachScreenReset + attachScreenReset,
			wantDeferred: "\r\n" + replayGapNotice + "\r\n",
		},
		{
			name:      "pipe gets no control bytes and an immediate notice",
			tty:       false,
			wantImmed: replayGapNotice + "\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			d := newAttachDisplay(&out, &errOut, tc.tty)
			if err := d.reset(); err != nil {
				t.Fatal(err)
			}
			if err := d.replayGap(gap); err != nil {
				t.Fatal(err)
			}
			if out.String() != tc.wantOut {
				t.Fatalf("stdout=%q want %q", out.String(), tc.wantOut)
			}
			if errOut.String() != tc.wantImmed {
				t.Fatalf("stderr during session=%q want %q", errOut.String(), tc.wantImmed)
			}
			errOut.Reset()
			d.printDeferredNotices()
			if errOut.String() != tc.wantDeferred {
				t.Fatalf("stderr after restore=%q want %q", errOut.String(), tc.wantDeferred)
			}
			if strings.Contains(out.String()+errOut.String(), "81871") {
				t.Fatal("sequence numbers leaked into user-facing output")
			}
		})
	}
}

func TestAttachDisplayNoGapNoNotice(t *testing.T) {
	var out, errOut bytes.Buffer
	d := newAttachDisplay(&out, &errOut, true)
	d.printDeferredNotices()
	if errOut.Len() != 0 {
		t.Fatalf("unexpected notice %q", errOut.String())
	}
}
