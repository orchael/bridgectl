# Local AI code review

This example lets a developer or another AI agent request a review of the branch currently checked out in a local repository. It starts a disposable bridgectl container, mounts the repository at `/repos/workspace`, injects only the selected provider credential, and sends a review-only prompt through bridgectl's non-TTY session mode.

## Run it

From the bridgectl checkout:

```bash
export OPENAI_API_KEY=...
export BRIDGECTL_REVIEW_PROVIDER=codex
./examples/code-review/run-review.sh /path/to/repository
```

Claude can be selected with `BRIDGECTL_REVIEW_PROVIDER=claude` and `CLAUDE_CODE_OAUTH_TOKEN`.

An AI agent can invoke exactly the same command after it has finished work on a branch. The script discovers the current branch from Git, so the calling agent does not need to construct GitHub API arguments.

The container is intentionally ephemeral. The checkout is mounted read/write because provider CLIs may need repository metadata, but the review prompt explicitly prohibits changes. For stronger isolation, a future variant can mount a copied worktree read-only.

## Agent handoff

A coding agent's final verification step can be:

```bash
/path/to/bridgectl/examples/code-review/run-review.sh "$PWD" > /tmp/code-review.md
```

Treat a non-zero exit as a failed reviewer run, not as review approval. The caller decides how to surface `/tmp/code-review.md`.

This is the local primitive used by Bosun. In Kubernetes, Bosun replaces `docker run` with a short-lived Job and injects GitHub/provider credentials from Kubernetes Secrets.
