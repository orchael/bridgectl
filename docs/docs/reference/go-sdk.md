---
title: Go SDK
---

The Go SDK lives in `pkg/bridgeclient`.

`GetSession` and each `ListSessions` entry include `GetInteraction()`: state,
source, capability flags, revision, timestamps, and an optional pending request.
See [Session Status](../guides/session-status.md#consume-status-from-any-client)
for a complete read example and compatibility behavior.

```bash
go get github.com/orchael/bridgectl/pkg/bridgeclient
```

## Local Client

```go
client, err := bridgeclient.New(
    bridgeclient.WithTarget("unix:///home/me/.config/bridgectl/server.sock"),
)
if err != nil {
    return err
}
defer client.Close()
```

Most local tools use `internal/localserver.DiscoverTarget` instead of hard-coding the socket path.

## Remote mTLS + JWT Client

```go
client, err := bridgeclient.New(
    bridgeclient.WithTarget("machine-b.tailnet-name.ts.net:9445"),
    bridgeclient.WithMTLS(bridgeclient.MTLSConfig{
        CABundlePath: "/home/me/.config/bridgectl/certs/step-ca-root.crt",
        CertPath:     "/home/me/.config/bridgectl/certs/machine-a.crt",
        KeyPath:      "/home/me/.config/bridgectl/certs/machine-a.key",
        ServerName:   "machine-b.tailnet-name.ts.net",
    }),
    bridgeclient.WithJWT(bridgeclient.JWTConfig{
        PrivateKeyPath: "/home/me/.config/bridgectl/certs/jwt-signing.key",
        Issuer:         "machine-a",
        Audience:       "bridge",
    }),
)
```

## Start and Attach

```go
resp, err := client.StartSession(ctx, &bridgev1.StartSessionRequest{
    ProjectId: "demo",
    SessionId: uuid.NewString(),
    RepoPath:  "/home/me/repos/bridge-demo",
    Provider:  "codex",
})
if err != nil {
    return err
}

stream, err := client.AttachSession(ctx, &bridgev1.AttachSessionRequest{
    SessionId: resp.GetSessionId(),
    ClientId:  "demo-writer",
    Role:      bridgev1.AttachRole_ATTACH_ROLE_WRITER,
})
if err != nil {
    return err
}
```

## Send Input

```go
_, err = client.WriteInput(ctx, &bridgev1.WriteInputRequest{
    SessionId: resp.GetSessionId(),
    ClientId:  stream.ClientID(),
    Data:      []byte("run the tests\n"),
})
```

## Stream Output

```go
err = stream.RecvAll(ctx, func(ev *bridgev1.AttachSessionEvent) error {
    switch ev.Type {
    case bridgev1.AttachEventType_ATTACH_EVENT_TYPE_OUTPUT:
        _, err := os.Stdout.Write(ev.Payload)
        return err
    case bridgev1.AttachEventType_ATTACH_EVENT_TYPE_REPLAY_GAP:
        return fmt.Errorf("replay gap: oldest=%d last=%d", ev.OldestSeq, ev.LastSeq)
    case bridgev1.AttachEventType_ATTACH_EVENT_TYPE_ERROR:
        return errors.New(ev.Error)
    }
    return nil
})
```
