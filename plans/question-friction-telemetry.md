# Question Friction Telemetry

## Goal

Measure how often interactive coding agents interrupt a human, distinguish useful questions from routine approvals, and export aggregated evidence that Ballast can use to reduce future interruptions.

The goal is **not** to bypass provider safety controls. The feedback loop should remove unnecessary questions at the instruction/policy layer and preserve questions whose answers change the outcome.

## Event model

`internal/telemetry` normalizes provider-specific terminal output into two correlated events:

- `question`: agent output that appears to request permission, confirmation, a choice, or clarification.
- `answer`: the next human input after a detected question.

Each question gets a stable fingerprint based on provider, class, and canonicalized text. Answers are classified as `accepted`, `rejected`, `changed`, or `unknown`.

Persist only redacted text by default. The JSONL sink creates files with mode `0600`. Do not upload raw transcripts automatically.

## Supervisor integration

Wire one `telemetry.Analyzer` into `bridge.Supervisor` so every provider uses the same instrumentation.

Integration points:

1. `Supervisor.Start`: retain `SessionID`, `ProjectID`, and selected provider in the telemetry session identity.
2. `appendChunk`: for `ChunkTypeOutput`, call `ObserveOutput` after ANSI stripping and before fan-out.
3. `WriteInput`: after writer authorization succeeds and before writing to the PTY/stdin, call `ObserveInput`.
4. `waitLoop`: flush/export any in-memory aggregate state if a sink needs it.

Telemetry failures must remain best-effort and must never fail or delay an agent session.

## Configuration

Add a `telemetry` block to bridgectl configuration:

```yaml
telemetry:
  enabled: true
  events_path: ~/.local/state/bridgectl/question-events.jsonl
  feedback_path: ~/.local/state/bridgectl/ballast-feedback.json
  include_redacted_text: true
```

Defaults:

- disabled until explicitly enabled during the initial rollout;
- local files only;
- redaction always enabled;
- no remote upload.

## Reporting

Add `bridgectl telemetry report` with these metrics:

- questions per agent-hour;
- questions per session;
- acceptance rate;
- rejection/change rate;
- median response latency;
- top recurring question fingerprints;
- avoidable-question candidates.

A question becomes an **avoidable candidate** only after enough evidence exists. Initial threshold:

- at least 5 observations;
- at least 90% accepted without modification;
- zero rejections in the most recent sample window.

Questions involving production changes, destructive operations, credentials/secrets, external publication, money/cost, or an explicit user choice must never become automatic approval candidates based only on frequency.

## Ballast feedback export

`telemetry.Feedback` is the contract between bridgectl and Ballast:

```json
{
  "schema_version": 1,
  "generated_at": "2026-09-13T17:00:00Z",
  "questions": [
    {
      "fingerprint": "1dba0e...",
      "class": "permission",
      "example": "Do you want me to run the unit tests?",
      "asked": 18,
      "accepted": 18,
      "rejected": 0,
      "changed": 0,
      "unknown": 0
    }
  ]
}
```

Ballast should turn high-confidence candidates into proposed instruction changes, never directly into provider bypass flags.

## Feedback loop

1. bridgectl records questions and human outcomes.
2. `bridgectl telemetry report` shows interruption cost and recurring fingerprints.
3. `bridgectl telemetry export --format ballast` writes the aggregate contract.
4. Ballast's question-friction review consumes the export and proposes a rule change.
5. A human reviews the Ballast change.
6. Updated Ballast rules roll out to repositories.
7. bridgectl measures whether the fingerprint disappears without increasing rejected/changed questions.

This makes question reduction measurable. The target is not zero questions; the target is near-zero **low-information** questions.

## Follow-up implementation

- Wire Analyzer into Supervisor at the integration points above.
- Add YAML config and CLI flags.
- Add `telemetry report` and `telemetry export` commands.
- Add provider fixtures for Claude Code and Codex permission prompts.
- Add tests proving telemetry failures cannot interrupt PTY I/O.
- Add a rolling window to distinguish old behavior from behavior after a Ballast rule update.
