# Lessons

## 2026-09-14 Secret Tests Must Not Embed Provider-Shaped Literals
- Incident/bug: GitHub push protection rejected a telemetry test commit because
  a synthetic AWS access-key fixture matched the real credential shape.
- Root cause pattern: A redaction test needs the runtime value to match, but a
  source-code literal is also scanned as though it could be a live secret.
- Preventative rule: Assemble provider-shaped redaction fixtures from benign
  source fragments at test runtime, and never bypass push protection for a test
  value.
- Validation added: The same AWS redaction branch remains covered while no
  AWS-shaped access-key literal exists in the committed source.

## 2026-09-14 Telemetry Reliability Requires Coupled Completion Boundaries
- Incident/bug: Process exit could precede the output reader's final chunk,
  flushing and delivery used independent timers, and a connected collector
  could withhold acknowledgements indefinitely.
- Root cause pattern: Related lifecycle signals were treated as independent
  events even though ordering between them defines completeness and latency.
- Preventative rule: End sessions only after readers drain, trigger delivery as
  part of successful sealing, and put a deadline around every remote delivery
  batch. Stream reports directly from retained JSONL so the disk budget does not
  become an equivalent heap-memory requirement.
- Validation added: Race tests cover reader/end ordering, post-flush delivery,
  missing acknowledgements, and streamed composite reports; the real Docker E2E
  verifies final provider output precedes the session-ended event.

## 2026-09-14 Distributed Session IDs Need a Producer Namespace
- Incident/bug: Telemetry correlation, sequence state, and reports keyed only by
  `session_id`, so independent bridges producing the same ID could combine
  unrelated conversations.
- Root cause pattern: An identifier unique inside one process is not globally
  unique after streams from multiple producers share a collector and storage
  boundary.
- Preventative rule: Give each producer a stable identity and key all in-memory
  and analytical session state by `(source_id, session_id)`. Persist generated
  identities atomically with private permissions, reject symlink identity files,
  and retain an explicit legacy namespace for old records.
- Validation added: Race tests cover cross-source correlation and sequences,
  report counts, persistent and concurrent UUID creation, invalid IDs, and
  symlink rejection; the live Docker E2E filters collector-volume events by the
  full composite identity.

## 2026-09-14 Full-Stream Telemetry Needs Valid Outer Test Fixtures
- Incident/bug: The first malformed-interaction collector test passed even
  though interaction metadata validation was absent because the test used
  segment IDs containing spaces and failed earlier at segment-ID validation.
- Root cause pattern: A negative test can produce the expected status through
  an unintended outer validation layer, leaving the target validation untested.
- Preventative rule: Keep envelopes valid when testing payload validation and
  assert the narrowest practical failure behavior. For full-stream telemetry,
  remove secrets before the async boundary, omit opaque invalid UTF-8, and never
  include raw input content in diagnostic logs.
- Validation added: Corrected segment IDs first reproduce acceptance of invalid
  stream metadata; focused tests now cover direction, stream, sequence, opaque
  content, collector-side secret defense, and bridge-side redaction.

## 2026-09-14 Durable Telemetry Needs Writable State at Both Boundaries
- Incident/bug: The first gRPC telemetry Docker E2E could start the collector
  but the non-root bridge failed before startup because its durable outbox was
  configured under a root-owned shared repository volume.
- Root cause pattern: Adding a durable delivery guarantee creates a new storage
  boundary on the producer as well as the collector; validating only the
  collector volume misses producer-side ownership and restart behavior.
- Preventative rule: Give durable outboxes a dedicated state volume, prepare
  mount ownership before dropping privileges, acknowledge only after collector
  fsync, and retain unacknowledged segments across transport failures.
- Validation added: Unit tests cover restart recovery, oldest-first eviction,
  idempotent replay, total collector outage, and S3 failure retention; the live
  Docker E2E runs the bridge and collector as non-root processes across separate
  state volumes.

## 2026-09-13 Go VCS Stamping in Temporary Worktrees
- Incident/bug: `make build` failed in the handed-off `/tmp` worktree because a
  non-repository `/tmp/.git` sandbox marker made Go run `git status` from
  `/tmp`, even though Git itself correctly resolved the linked worktree.
- Root cause pattern: Automatic Go VCS stamping can discover a misleading
  ancestor marker in temporary/sandboxed worktree layouts.
- Preventative rule: Repository builds that already inject an explicit version
  through `-ldflags` should disable implicit VCS stamping so supported worktree
  paths build reproducibly.
- Validation added: `make build` now disables implicit VCS stamping and uses the
  repository's writable `/tmp` Go-cache fallback, without requiring caller
  environment workarounds.

## 2026-09-13 Telemetry Privacy and Stream Boundaries
- Incident/bug: The initial question telemetry redactor changed
  `Authorization: Bearer secret` to `Authorization=[REDACTED] secret`, leaving
  the credential in persisted JSONL, while fingerprints were calculated before
  redaction and existing event files retained permissive modes.
- Root cause pattern: A positive "text changed" assertion is weaker than proving
  sensitive values are absent, and file creation modes do not harden an
  existing path. Telemetry APIs can also appear chunk-safe even though PTY I/O
  does not preserve logical prompt/answer boundaries.
- Preventative rule: Assert known secrets are absent from every persisted and
  exported representation, redact before fingerprinting, chmod an opened local
  telemetry file before writing, and explicitly assign PTY framing/deduplication
  to the live integration layer.
- Validation added: A black-box e2e test covers question → answer → private
  JSONL → aggregate feedback, while unit tests cover redaction, permissions,
  classification, decisions, concurrency, errors, UTF-8 truncation, and 99.0%
  package coverage.

## 2026-09-13 Boundary-Tight E2E Timeouts
- Incident/bug: The repo-setup environment propagation e2e intermittently timed
  out under the parallel race suite while passing immediately in isolation.
- Root cause pattern: A five-second operation timeout left no scheduling margin
  under load, making a non-timing test depend on a boundary-tight deadline.
- Preventative rule: Give process-based e2e setup a bounded but meaningful
  margin below its parent context; do not set an incidental operation timeout
  equal to the observed slow-path duration.
- Validation added: The test-only setup/context budgets are now 10/20 seconds;
  the focused race test and full parallel race suite cover the change.

## 2026-09-11 Remote Step CA E2E False Success
- Incident/bug: Client enrollment requested `admin` although the test CA created `bridge-jwk`; the Make target still printed `PASSED` after the client exited 1.
- Root cause pattern: `docker wait` prints the container status to stdout and can return success itself for a failed container. Detached setup errors were also overwritten, and default Compose `ps` can omit an already-exited client.
- Follow-on failures: The client dialed `bridge-server` although the server certificate covered `server`; SDK tests expected a local CA bundle that client init does not create when given an external root path.
- Preventative rule: Align enrollment with CA initialization, dial a certificate SAN, supply the SDK trust bundle, capture the container status explicitly, include stopped containers during lookup, and preserve setup errors through cleanup.
- Validation added: `TestMakeTargetResult` exercises success, client failure, early exit, build/startup/lookup/wait errors, missing containers, and invalid wait output using a fake Docker executable against the real Make target.

## 2026-09-10 CLI Takeover Stream Ordering
- Incident/bug: `session attach --take-over` claimed the writer slot before opening its observer stream, returning `permission denied`.
- Root cause pattern: SDK `AttachSession` constructs a lazy wrapper; `RecvAll` performs the actual RPC. Existing handoff tests attached through the SDK and never exercised CLI ordering.
- Preventative rule: Gate dependent session RPCs on the server's `ATTACHED` event and test operator workflows through the real CLI with a PTY.
- Validation added: CLI takeover regression reproduces the original error, then verifies writer transfer, input, observer continuity, and detach; rejected-claim coverage checks error propagation and input/resize gating.

## 2026-03-30 PTY Transport Test Execution
- Incident/bug: A provider unit test that executed a temp script failed inside the sandbox with `operation not permitted`.
- Root cause pattern: Tests that shell out in this environment can fail for sandbox reasons unrelated to application logic.
- Early signal missed: The first version of the startup-probe test assumed subprocess execution was always allowed in unit tests.
- Preventative rule: Keep unit tests for provider construction/path assembly pure where possible; reserve subprocess behavior checks for integration/e2e layers.
- Validation added (test/check/alert): Replaced the exec-heavy unit test with a pure command-construction test and kept PTY behavior validation in the higher-level smoke path.

## 2026-04-23 Apt Package Smoke Harness
- Incident/bug: The first Debian package smoke test installed successfully but still failed the health check.
- Root cause pattern: Docker port publishing cannot reach a service that is intentionally bound to `127.0.0.1` inside the container.
- Early signal missed: The packaged default config is localhost-only by design, but the first smoke harness assumed host-to-container access over a published port.
- Preventative rule: When a packaged service defaults to loopback-only binding, run health verification inside the target environment or through an explicit tunnel instead of relying on Docker port publishing.
- Validation added (test/check/alert): Updated `scripts/smoke-apt-local.sh` to execute the gRPC healthcheck inside each Ubuntu container, matching packaged service behavior.

## 2026-08-14 Detached Docker Smoke Setup
- Incident/bug: The provider runtime smoke test tried to exec into a detached Ubuntu container before package/user setup had completed.
- Root cause pattern: `docker run -d` returns after the container starts, not after an inline bootstrap script reaches its steady state.
- Early signal missed: The first harness assumed a missing packaged file meant package contents were wrong, but the generated `.deb` contained the file.
- Preventative rule: Detached container smoke tests must create and wait on an explicit readiness marker before running assertions or follow-up exec commands.
- Validation added (test/check/alert): `scripts/smoke-provider-runtime-user.sh` waits for `/tmp/provider-runtime-smoke-ready` before running the non-root provider runtime installer.

## 2026-08-16 Docker Config Certificate Name Alignment
- Incident/bug: Changing the container default config to `bridge-docker.yaml` initially made default image startup fail because the entrypoint generated `bridge.crt` while the Docker config expected `bridge.local.crt`.
- Root cause pattern: Docker entrypoint-generated filenames are part of the config contract; changing the default config without checking generated certificate names creates a startup-only failure.
- Early signal missed: Compose overrides had been setting `BRIDGE_CN=bridge.local`, masking the mismatch in normal compose-based development.
- Preventative rule: When changing default container config or certificate CN defaults, run a no-args detached image startup smoke and verify the container stays running.
- Validation added (test/check/alert): `docker run -d --name issue180-default bridgectl:issue-180` stayed running after aligning `BRIDGE_CN`, `BRIDGE_CLIENT_CN`, and SAN defaults with `bridge-docker.yaml`.

## 2026-08-16 Live Provider TUI E2E Input
- Incident/bug: The first unprotected-mode e2e harness treated echoed prompt text as completion and sent Claude's prompt plus Enter in one PTY write, leaving Claude's TUI composer unsubmitted.
- Root cause pattern: Interactive provider CLIs echo prompts and can treat pasted text differently from a separate submit key, so transcript literals alone are not a reliable proof of action.
- Early signal missed: Codex and Claude transcripts showed the prompt in the composer with zero tokens, but the test advanced because the completion marker was present in the echoed prompt.
- Preventative rule: For live provider e2e tests, prove behavior through external state first, then use transcript markers only as secondary evidence; send provider-specific submit keys as separate PTY writes when needed.
- Validation added (test/check/alert): `env-secrets aws -s /bridgectl/e2e -- make test-e2e-unprotected` passed with protected and unprotected Codex/Claude `.git` marker checks.

## 2026-09-13 Live Telemetry Must Be Off the Session I/O Path
- Incident/bug: Direct persistence from PTY callbacks would let a slow or full
  telemetry sink delay the provider session, and naive `?` framing interpreted
  ANSI private-mode sequences such as `ESC[?25h` as questions.
- Root cause pattern: Observability code inherits production latency and stream
  semantics unless queue bounds, drop behavior, framing, and shutdown ownership
  are explicit.
- Preventative rule: Copy authorized I/O into a bounded non-blocking collector,
  treat telemetry failure as diagnostic-only, skip delimiters inside terminal
  control sequences, and flush within a fixed shutdown deadline.
- Validation added (test/check/alert): Race tests cover slow/full/failing sinks,
  fragmented Claude/Codex fixtures, ANSI framing, authorized input, and bounded
  close; a real Docker bridge e2e validates the persisted correlation.

## 2026-09-13 Telemetry Retention Belongs at the Persistence Boundary
- Incident/bug: A single append-only JSONL file had no upper bound, and a
  question-specific filename made future filtered event types misleading.
- Root cause pattern: A bounded in-memory queue protects session latency but
  does not bound durable storage; retention and event selection are independent
  controls.
- Preventative rule: Filter normalized events before queueing or delivery,
  rotate at the sink under its write lock, cap retained generations, and make
  readers consume rotations oldest-first.
- Validation added (test/check/alert): Unit tests force multiple rotations and
  verify ordered reads and kind exclusion; the Docker e2e crosses a distinct
  collector service and reads only `question` and `answer` from its volume.

## 2026-09-14 Telemetry Retention Must Use a Disk Budget
- Incident/bug: A one-second flush interval combined with a 128-segment limit
  evicted collector-volume telemetry after roughly two minutes even though the
  stored segments used only a few megabytes.
- Root cause: Delivery cadence created many small immutable segments, while the
  retention default was expressed only as a count and was not documented as a
  disk-capacity budget.
- Preventative rule: Bound local retention directly by aggregate bytes rather
  than deriving capacity from a file count. Include active and immutable files,
  enforce a smaller budget on restart, and expose human-readable SI/IEC sizes.
- Validation added: Config tests assert the 10-second bridge flush default;
  spool tests cover byte eviction and recovery, and the CLI/Compose tests assert
  the default `1GB` disk budget.

## 2026-09-14 Telemetry Boundaries Are Security and Consistency Boundaries

- Incident/bug: Per-read redaction could miss credentials and UTF-8 code points
  split across transport chunks, while reports could race a mutable active file.
- Root cause pattern: Transport reads and filesystem writes are not semantic
  record boundaries; treating them as complete records leaks implementation
  timing into privacy and analysis correctness.
- Preventative rule: Reassemble bounded interaction records before redaction,
  sequence only retained events, and expose only newline-complete immutable
  segments to report readers.
- Validation added: Split UTF-8 and multi-chunk secret regressions, filtered
  sequence assertions, active-segment exclusion, partial-write recovery, and a
  live collector E2E ending with `session_ended`.

## 2026-09-15 Telemetry Protocols Need Semantic Coverage

- Incident/bug: Assignment redaction handled shell-style values but missed
  quoted JSON, ANSI stripping handled CSI controls but exposed OSC/DCS payloads,
  question state became visible before its event was persisted, and each
  delivery pass recreated a stream despite the long-lived-stream contract.
- Root cause pattern: Tests covered common representations and single-threaded
  happy paths without asserting equivalent wire formats, persistence ordering,
  or connection lifetime across batches.
- Preventative rule: At privacy and transport boundaries, test multiple
  encodings, state visibility versus durable event order, and reuse across at
  least two separately sealed batches.
- Validation added: Quoted-JSON secret, OSC/DCS, concurrent correlation, and
  two-batch one-stream regressions plus the live bridge-to-volume Docker E2E.
- Collector defense-in-depth reapplies redaction, so redaction must be
  idempotent and quoted values must be consumed through their closing quote.
  Add second-pass equality assertions and a real collector proof with a
  whitespace-containing JSON secret.
- Keep semantic-framer bounds separate from corpus-record bounds, expose any
  exceptional omission explicitly, and exclude nested generated dependencies
  from Docker contexts. On constrained hosts, build independent Go images
  sequentially to reduce transient disk demand.
