# Supervised agent orchestrator example

This example is a tab in the existing `examples/web` application. It reuses the existing HTTP API for Bridgectl sessions rather than creating a second session manager.

## Run

Start the Bridgectl daemon and the example web server using the existing instructions for `examples/web`. Open the web app and select **Orchestrator example**. Supply an absolute repository path, choose a provider, enter an objective, and click **Start agent**. Once the session's writer stream reports ready, click **Send objective + policy**.

The app streams terminal output and looks for the convention `APPROVAL_REQUIRED: <action and reason>`. When detected, the user can approve, deny, or stop. An agent can signal `TASK_COMPLETE: <summary>`; the user should review its output and stop the session. The **Refresh status** button checks the daemon's actual session list.

## State machine

`draft -> starting -> running -> awaiting-approval -> running -> stopping -> stopped`. Failed requests enter `failed`. The user can stop the agent from running or awaiting approval.

## Scope and security

This is an instructional prototype, not a production policy engine. Terminal-output markers are advisory and can be spoofed by agent output, files, or tools. An agent may act before requesting approval. There is no durable task store, multi-agent scheduler, retry manager, authoritative completion signal, or isolation between repository operations. Browser refresh loses orchestration state but does not necessarily terminate the Bridgectl session.

For production: enforce tool-level permissions outside the prompt, bind approval to a specific authenticated user and action, persist decisions and audit records server-side, enforce execution budgets and timeouts, supervise session recovery, and restrict repository and secret access. Never expose this web UI publicly without authentication and authorization. The existing session lifecycle is real; policy enforcement is not.
