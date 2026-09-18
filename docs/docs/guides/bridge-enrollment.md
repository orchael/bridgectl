# Bridge enrollment

Bridge is optional. A bridgectl installation remains fully usable in standalone mode until `bridgectl login` is run.

## Log in

The production default is `https://bridge.orchael.com`. Development and end-to-end testing can select the development service explicitly:

```sh
BRIDGECTL_BRIDGE_URL=https://bridge.orchael.dev bridgectl login
```

The equivalent flag is `bridgectl login --bridge https://bridge.orchael.dev`. The flag takes precedence over `BRIDGECTL_BRIDGE_URL`, which takes precedence over a previously saved enrollment URL, followed by the production default.

Login uses Bridge's device authorization flow. bridgectl displays a short code, opens `/device` when a local browser opener is available, and polls until the browser approval succeeds. Google authentication and organization selection happen in Bridge; bridgectl never receives Google tokens, passwords, cookies, or Auth.js sessions.

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

`whoami` reports the Bridge origin, organization and installation identifiers, and login status. `doctor` reports enrollment and telemetry configuration without secrets. `logout` removes local enrollment metadata and secret material while leaving standalone bridgectl configuration untouched. The current Bridge device protocol does not expose an installation revocation endpoint to the telemetry-only credential, so logout reports that remote revocation is unavailable rather than claiming it occurred.

## Development integration

Normal sessions use the existing collector and spool. When enrollment supplies a telemetry endpoint and no explicit collector is configured, the collector sends HTTPS segment uploads with `Authorization: Bearer brc_...` to that endpoint. TLS verification remains enabled; HTTP downgrade and URL query/fragment origins are rejected.
