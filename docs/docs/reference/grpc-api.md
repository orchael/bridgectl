---
title: gRPC API
---

The API is defined in `proto/bridge/v1/bridge.proto`. Generated Go stubs live under `gen/bridge/v1` and should not be edited by hand.

## Service

```protobuf
service BridgeService {
  rpc StartSession(StartSessionRequest) returns (StartSessionResponse);
  rpc StopSession(StopSessionRequest) returns (StopSessionResponse);
  rpc GetSession(GetSessionRequest) returns (GetSessionResponse);
  rpc ListSessions(ListSessionsRequest) returns (ListSessionsResponse);
  rpc DiagnoseSession(DiagnoseSessionRequest) returns (DiagnoseSessionResponse);
  rpc AttachSession(AttachSessionRequest) returns (stream AttachSessionEvent);
  rpc WriteInput(WriteInputRequest) returns (WriteInputResponse);
  rpc ResizeSession(ResizeSessionRequest) returns (ResizeSessionResponse);
  rpc ClaimWriter(ClaimWriterRequest) returns (ClaimWriterResponse);
  rpc ReleaseWriter(ReleaseWriterRequest) returns (ReleaseWriterResponse);
  rpc Health(HealthRequest) returns (HealthResponse);
  rpc ListProviders(ListProvidersRequest) returns (ListProvidersResponse);
  rpc RegisterJWTKey(RegisterJWTKeyRequest) returns (RegisterJWTKeyResponse);
}
```

## Session Lifecycle

`StartSession` requires:

| Field | Meaning |
| --- | --- |
| `project_id` | Logical project for authorization and filtering. |
| `session_id` | Caller-generated session ID. |
| `repo_path` | Absolute path on the bridge host. |
| `provider` | `claude`, `codex`, `opencode`, or `gemini`. |
| `initial_cols`, `initial_rows` | Optional initial PTY size. |

`DiagnoseSession` returns the bounded, schema-versioned diagnostic snapshot of a
session as one JSON document (`report_json`; field reference under
`session diagnose` in the CLI reference). It is authorized like `GetSession` and
returns `NOT_FOUND` for an unknown session.

`StopSession` stops a running session. `force` skips graceful shutdown.

## Attach Events

`AttachSession` streams `AttachSessionEvent` values:

| Event type | Meaning |
| --- | --- |
| `ATTACHED` | Confirms the client is attached to the session. |
| `OUTPUT` | Raw PTY bytes in `payload`. |
| `REPLAY_GAP` | Requested replay range is no longer fully buffered. |
| `SESSION_EXIT` | Provider process exited. |
| `ERROR` | Session or stream error text. |
| `THINKING` | A thinking block emitted by the provider. |
| `WRITER_CLAIMED` | A client claimed the writer slot. |
| `WRITER_RELEASED` | A client released the writer slot. |

## grpcurl

Local unauthenticated Unix socket mode is easiest through the Go SDK or CLI. For explicit TCP mTLS calls:

```bash
grpcurl \
  -cacert certs/ca-bundle.crt \
  -cert certs/dev-client.crt \
  -key certs/dev-client.key \
  -servername bridge.local \
  -import-path proto \
  -proto bridge/v1/bridge.proto \
  127.0.0.1:9445 \
  bridge.v1.BridgeService/Health
```

JWT-authenticated calls are easier through `bridgeclient` because the SDK mints short-lived JWTs for each RPC.
