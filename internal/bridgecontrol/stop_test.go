package bridgecontrol

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestStopDurableClaimAndScope(t *testing.T) {
	org, inst := uuid.NewString(), uuid.NewString()
	calls := 0
	cfg := Config{Credential: "test", CommandPath: t.TempDir(), StopFunc: func(_ context.Context, req StopRequest) error { calls++; return errors.New("uncertain") }}
	request := StopRequest{ID: uuid.NewString(), OrganizationID: org, InstallationID: inst, UserID: uuid.NewString(), SessionID: uuid.NewString(), ExpiresAt: time.Now().Add(15 * time.Second)}
	c := New(cfg)
	c.organizationID, c.installationID = org, inst
	wrong := request
	wrong.InstallationID = uuid.NewString()
	if r := c.executeStop(context.Background(), wrong); r.Code != "wrong_installation" || calls != 0 {
		t.Fatalf("wrong installation: %+v, calls %d", r, calls)
	}
	if r := c.executeStop(context.Background(), request); r.Status != "unknown" || calls != 1 {
		t.Fatalf("first: %+v, calls %d", r, calls)
	}
	c = New(cfg)
	c.organizationID, c.installationID = org, inst
	if r := c.executeStop(context.Background(), request); r.Status != "unknown" || calls != 1 {
		t.Fatalf("restart replay: %+v, calls %d", r, calls)
	}
	changed := request
	changed.SessionID = uuid.NewString()
	if r := c.executeStop(context.Background(), changed); r.Code != "idempotency_conflict" || calls != 1 {
		t.Fatalf("changed replay: %+v, calls %d", r, calls)
	}
}

func TestStopAcceptedAndExpired(t *testing.T) {
	org, inst := uuid.NewString(), uuid.NewString()
	calls := 0
	c := New(Config{Credential: "test", CommandPath: t.TempDir(), StopFunc: func(context.Context, StopRequest) error { calls++; return nil }})
	c.organizationID, c.installationID = org, inst
	p := StopRequest{ID: uuid.NewString(), OrganizationID: org, InstallationID: inst, UserID: uuid.NewString(), SessionID: uuid.NewString(), ExpiresAt: time.Now().Add(time.Second)}
	if r := c.executeStop(context.Background(), p); r.Status != "accepted" {
		t.Fatalf("accepted: %+v", r)
	}
	if r := c.executeStop(context.Background(), p); r.Status != "accepted" || calls != 1 {
		t.Fatalf("replay: %+v, calls %d", r, calls)
	}
	p.ID = uuid.NewString()
	p.ExpiresAt = time.Now().Add(-time.Second)
	if r := c.executeStop(context.Background(), p); r.Code != "expired" || calls != 1 {
		t.Fatalf("expired: %+v, calls %d", r, calls)
	}
}
