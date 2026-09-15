package telemetry

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestLocalSpoolingSinkSealsOnFlushInterval proves TEL-101 and TEL-110: local
// reports see low-volume events without waiting for shutdown or size rotation.
func TestLocalSpoolingSinkSealsOnFlushInterval(t *testing.T) {
	spool, err := NewSegmentSpool(t.TempDir(), 1<<20, 10<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	sink := NewLocalSpoolingSink(spool, 20*time.Millisecond, nil)
	defer func() {
		if err := sink.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	if err := sink.Record(validCollectorEvent(EventQuestion)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool {
		events, err := ReadEvents(spool.Dir())
		return err == nil && len(events) == 1
	})
	if err := sink.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := sink.Record(validCollectorEvent(EventQuestion)); !errors.Is(err, ErrSinkClosed) {
		t.Fatalf("Record after close error=%v, want ErrSinkClosed", err)
	}
}
