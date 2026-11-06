package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	bridgev1 "github.com/orchael/bridgectl/gen/bridge/v1"
	"github.com/orchael/bridgectl/pkg/bridgeclient"
)

func TestAttachStreamErrorMapping(t *testing.T) {
	const sid = "434470ed-0000-4000-8000-000000000000"
	conflict := status.Error(codes.AlreadyExists, "attach session: session already has an active writer")
	tests := []struct {
		name     string
		err      error
		remote   string
		want     []string
		notWant  []string
		conflict bool
	}{
		{
			name:     "grpc already exists",
			err:      conflict,
			want:     []string{"Another client is controlling this session.", "bridgectl session attach --observe " + sid, "bridgectl session attach --take-over " + sid},
			notWant:  []string{"rpc error", "AlreadyExists", "code ="},
			conflict: true,
		},
		{
			name:     "wrapped status",
			err:      fmt.Errorf("stream: %w", conflict),
			want:     []string{"Watch instead:", "Take control:"},
			conflict: true,
		},
		{
			name:     "sdk sentinel with remote",
			err:      bridgeclient.ErrSessionAlreadyExists,
			remote:   "host.example:9445",
			want:     []string{"bridgectl session attach --remote host.example:9445 --observe " + sid, "--remote host.example:9445 --take-over " + sid},
			conflict: true,
		},
		{
			name: "other errors keep their detail",
			err:  status.Error(codes.Unavailable, "connection refused"),
			want: []string{"attach: ", "connection refused"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := attachStreamError(tc.err, sid, tc.remote)
			if isWriterConflict(tc.err) != tc.conflict {
				t.Fatalf("isWriterConflict=%v want %v", !tc.conflict, tc.conflict)
			}
			for _, want := range tc.want {
				if !strings.Contains(got.Error(), want) {
					t.Fatalf("message %q missing %q", got, want)
				}
			}
			for _, bad := range tc.notWant {
				if strings.Contains(got.Error(), bad) {
					t.Fatalf("message %q contains %q", got, bad)
				}
			}
			if !tc.conflict && !errors.Is(got, tc.err) {
				t.Fatal("non-conflict error lost its cause")
			}
		})
	}
}

func TestWriterTrackerMessages(t *testing.T) {
	claimed := func(id string) *bridgev1.AttachSessionEvent {
		return &bridgev1.AttachSessionEvent{Type: bridgev1.AttachEventType_ATTACH_EVENT_TYPE_WRITER_CLAIMED, WriterClientId: id}
	}
	released := func(id string) *bridgev1.AttachSessionEvent {
		return &bridgev1.AttachSessionEvent{Type: bridgev1.AttachEventType_ATTACH_EVENT_TYPE_WRITER_RELEASED, WriterClientId: id}
	}
	const self, other = "4dbc0cb4-03a2-49a1-bb8c-657d221dfbdd", "b7c0a1d2-0000-4000-8000-000000000000"
	tests := []struct {
		name    string
		tracker writerTracker
		events  []*bridgev1.AttachSessionEvent
		want    []string
	}{
		{
			name:    "take-over claimer",
			tracker: writerTracker{self: self, claiming: true},
			events:  []*bridgev1.AttachSessionEvent{released(other), claimed(self)},
			want:    []string{"", msgYouControl},
		},
		{
			name:    "writer that lost control",
			tracker: writerTracker{self: self, controlling: true},
			events:  []*bridgev1.AttachSessionEvent{released(self), claimed(other)},
			want:    []string{"", msgYouLostControl},
		},
		{
			name:    "observer sees control change",
			tracker: writerTracker{self: self},
			events:  []*bridgev1.AttachSessionEvent{released(other), claimed(other)},
			want:    []string{msgControlFreed, msgOtherControls},
		},
		{
			name:    "unrelated event",
			tracker: writerTracker{self: self},
			events:  []*bridgev1.AttachSessionEvent{{Type: bridgev1.AttachEventType_ATTACH_EVENT_TYPE_OUTPUT}},
			want:    []string{""},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr := tc.tracker
			for i, ev := range tc.events {
				got := tr.message(ev)
				if got != tc.want[i] {
					t.Fatalf("event %d: got %q want %q", i, got, tc.want[i])
				}
				if strings.Contains(got, self) || strings.Contains(got, other) {
					t.Fatalf("client ID leaked: %q", got)
				}
			}
		})
	}
}
