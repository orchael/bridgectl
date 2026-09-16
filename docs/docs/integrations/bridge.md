# Bridge integration

Bridge is an optional hosted or self-hosted control plane for bridgectl. It does not replace bridgectl's standalone operation.

## Design principles

- A bridgectl installation works without a Bridge account or network access to Bridge.
- Existing local Unix-socket, TLS, mTLS/JWT, user-managed step-ca, local PKI, raw gRPC, Go SDK, Docker/Kubernetes/apt, and example/web workflows remain supported.
- Bridge configuration is additive and namespaced. It must not silently replace existing standalone security configuration.
- `https://bridge.orchael.com` is the default hosted service, not a protocol constant. Self-hosted Bridge instances expose the same discovery and enrollment protocol at another base URL.

## Proposed enrollment UX

```text
bridgectl login
bridgectl login --bridge https://bridge.example.com
```

The login command discovers the Bridge enrollment endpoints from the configured base URL, starts a browser/device authorization flow, and displays a URL/code fallback. After the user authenticates and chooses an organization, bridgectl enrolls the local machine and stores the resulting machine identity securely.

Bridge-managed machine certificates are short-lived and renewed automatically. `bridgectl logout` (or the final equivalent command) revokes/removes Bridge enrollment without damaging standalone configuration.

## Identity boundaries

Bridge uses separate identities for users, machines, and telemetry sources:

- user identity is authenticated in the Bridge browser application;
- machine identity belongs to one enrolled bridgectl installation and uses Bridge-managed step-ca credentials;
- telemetry source identity is provisioned/bound by Bridge and cannot be selected by collector payload labels.

## Control plane

When Bridge-managed mode is enabled, bridgectl can establish an outbound authenticated control connection to its configured Bridge instance. The connection publishes machine/session lifecycle state and receives authorized control commands. It is an adapter around the existing bridgectl Supervisor/session APIs, not a second agent runtime.

The control plane is separate from analytics telemetry. A Bridge connection is not required for local/direct clients.

## Compatibility contract

The following must remain valid after Bridge support lands:

```text
bridgectl server start
```

with no Bridge configuration, including the existing example/web application. Existing user-operated step-ca deployments remain first-class supported configurations.

See Linear MAR-70 and MAR-71 for implementation and test requirements.
