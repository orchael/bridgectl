# Bridge enrollment

Bridge is optional. A bridgectl installation remains fully usable in standalone mode until `bridgectl login` is run.

## Log in

The production default is `https://bridge.orchael.com`. Development and end-to-end testing can select the development service explicitly:

```sh
BRIDGECTL_BRIDGE_URL=https://bridge.orchael.dev bridgectl login
```

The equivalent flag is `bridgectl login --bridge https://bridge.orchael.dev`. The flag takes precedence over `BRIDGECTL_BRIDGE_URL`, which takes precedence over a previously saved enrollment URL, followed by the production default.

Login uses Bridge's device authorization flow. bridgectl displays a short code, opens `/device` when a local browser opener is available, and polls until the browser approval succeeds. Google authentication and organization selection happen in Bridge; bridgectl never receives Google tokens, passwords, cookies, or Auth.js sessions.

Polling honors the documented protocol exactly: on `slow_down`, bridgectl keeps the greatest of its current interval, the server-returned `interval`, and the `Retry-After` header, and never reduces it; `authorization_pending` continues at the current interval; `access_denied`, `expired_token`, and `invalid_grant` stop the login with that error. The successful exchange response carries an `api_version` field (currently `v1`); bridgectl rejects an enrollment whose `api_version` it does not recognize instead of silently persisting a credential issued under an incompatible protocol.

To skip Bridge's organization picker when the target organization is already known, pass its name (not an ID — bridgectl has no way to know Bridge's internal organization IDs):

```sh
bridgectl login --organization "Acme Inc"
# or
BRIDGECTL_ORGANIZATION="Acme Inc" bridgectl login
```

`--organization` takes precedence over `BRIDGECTL_ORGANIZATION`. Bridge only honors this hint when the signed-in user is a member of an organization with that exact (case-insensitive) name — it is never a substitute for real membership, and any other case (no hint, no match, or more than one membership and no hint) falls back to Bridge auto-creating a home organization for brand-new users or showing its own picker when there's a genuine choice to make.

After approval, bridgectl stores enrollment metadata in `bridge-enrollment.json` and the telemetry-only `brc_` credential in `bridge-credentials.json` beneath the bridgectl state directory. Both files are restricted to the current user. The credential is never printed or included in `whoami` or `doctor` output.

Login writes Bridge telemetry settings only when an explicit telemetry collector URL is not already configured. Existing standalone telemetry settings therefore remain authoritative.

## Status and logout

```sh
bridgectl whoami
bridgectl doctor
bridgectl logout
```

`whoami` reports the Bridge origin, the organization (name when Bridge provides one, otherwise its identifier, plus a link to the organization when Bridge returns one) and installation identifiers, and login status. `doctor` reports enrollment, a best-effort network reachability check against the enrolled Bridge origin, and telemetry configuration, all without secrets. `logout` rewrites any Bridge-managed telemetry settings before removing local enrollment metadata and secret material, so a failure to update `bridge.yaml` fails the logout loudly instead of leaving it pointing at a deleted credential; standalone bridgectl configuration is left untouched. The current Bridge device protocol does not expose an installation revocation endpoint to the telemetry-only credential, so logout reports that remote revocation is unavailable rather than claiming it occurred.

## Development integration

Normal sessions use the existing collector and spool. When enrollment supplies a telemetry endpoint and no explicit collector is configured, the collector sends HTTPS segment uploads with `Authorization: Bearer brc_...` to that endpoint. TLS verification remains enabled; HTTP downgrade and URL query/fragment origins are rejected.
