package bridgecontrol

import (
	"testing"

	"github.com/orchael/bridgectl/internal/bridge"
)

func TestMAR95ObservationScope(t *testing.T) {
	calls := 0
	c := New(Config{ObserveFunc: func(id string, after uint64, events, bytes int) (bridge.ActivityWindow, error) {
		calls++
		if id != "s" || after != 127 {
			t.Fatal("wrong cursor")
		}
		return bridge.ActivityWindow{NextSequence: 128}, nil
	}})
	c.organizationID = "o"
	c.installationID = "i"
	r := ObservationRequest{ID: "request", OrganizationID: "other", InstallationID: "i", SessionID: "s", AfterSequence: 127}
	if got := c.observe(r); got.Code != "invalid_request" || calls != 0 {
		t.Fatal(got)
	}
	r.OrganizationID = "o"
	got := c.observe(r)
	if got.ID != r.ID || got.Window.NextSequence != 128 || got.Code != "" || calls != 1 {
		t.Fatal(got)
	}
}
