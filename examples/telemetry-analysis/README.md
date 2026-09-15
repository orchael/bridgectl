# Telemetry analysis example

This bounded example demonstrates three extension points without turning
`bridgectl` into an analytics product:

- deterministic metrics and evidence-backed findings;
- read-only HTTP endpoints under `/api/analytics/`;
- a redacted, size-bounded LLM input packet that excludes thinking streams.

It does not call a model, authenticate users, persist derived analytics, or
provide a UI. Those belong in an orchestrator or analytics application.

Reconstructed turns use UTF-8-safe chunks of at most 64 KiB. `chunk_index` is
zero for the first chunk of a logical turn, and `continues` marks a size-sealed
chunk; chunks may share an event sequence. Metrics count logical turns, not
continuation chunks. Sequence gaps end a turn rather than presenting missing
evidence as continuous text. The LLM packet is limited to 4,000 Unicode
characters and 20 evidence entries, and excludes thinking streams.

Run it against an immutable collector segment directory:

```sh
go run ./examples/telemetry-analysis \
  --events ./var/telemetry/segments \
  --listen 127.0.0.1:9470
curl http://127.0.0.1:9470/api/analytics/summary
curl http://127.0.0.1:9470/api/analytics/findings
curl http://127.0.0.1:9470/api/analytics/sessions
```

For a single session, use its composite identity. Use `_legacy` for a v1 event
that has no source ID:

```sh
curl http://127.0.0.1:9470/api/analytics/sessions/bridge-a/session-123
curl http://127.0.0.1:9470/api/analytics/llm-packet/bridge-a/session-123
```
