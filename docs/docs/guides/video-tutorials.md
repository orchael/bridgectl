---
title: Video tutorials
sidebar_position: 12
---

# Video tutorials

These tutorials will be generated and verified by [Orchael Director](https://github.com/orchael/director).

## Planned recordings

| Tutorial | CLI workflow | Status |
| --- | --- | --- |
| List sessions | `bridgectl session list` | Metadata example ready |
| Start a session | `bridgectl session start --provider codex` | Planned |
| Diagnose a session | `bridgectl session diagnose` | Planned |
| Check local health | `bridgectl doctor` | Planned |
| Check Bridge enrollment | `bridgectl whoami` | Planned |
| Telemetry reports | `bridgectl telemetry report` | Planned |

Videos must pass assertions against isolated fixtures before publication.
Never record real credentials or production sessions.

## Source of truth

Commands and flags come from the Cobra CLI in `cmd/bridgectl`.
Tutorial definitions and generated artifacts are versioned separately.
Each published video will include a source revision, transcript, and captions.
