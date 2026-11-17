# Local Keycloak login

Local development runs without OIDC by default:

```sh
make kind-up          # kind and Crossplane only
make run-controller  # separate terminal
make run             # API, foreground
make run-ui          # UI, separate terminal (or cd ui && npm run dev)
```

The `auth` block in `hack/config.local.yaml` is commented out. The API treats
an absent block as authentication disabled; the UI development launcher reads
the same block and disables login even if an older `.env.local` enabled it.
Neither application command starts Keycloak or Docker.

To opt in:

1. Run `make keycloak` separately to configure/start the local identity provider.
2. Uncomment the complete `auth` block in `hack/config.local.yaml`.
3. Restart the API and UI with the same commands above.

To disable login, comment out that block again and restart both applications.
The Keycloak container may remain running independently. Configuration changes
do not restart existing processes or reuse a stale API listener.

`LOCAL_CONFIG=/path/to/config.yaml make run` and
`LOCAL_CONFIG=/path/to/config.yaml make run-ui` select another shared config.
For direct `npm run dev`, `CV_CONFIG` is resolved relative to the repository
root. Missing/invalid config and incomplete enabled OIDC fail startup.
Production does not read this local file: it still requires explicit
`AUTH_ENABLED` and server-side credentials.

Keycloak is a sibling Docker container, `cluster-vending-keycloak`, using
`quay.io/keycloak/keycloak:26.8.2`. It binds only `127.0.0.1:18080`. Its named
volume, `cluster-vending-keycloak-data`, holds Keycloak's own development H2
storage; platform state still lives in Kubernetes.

| Setting | Value |
|---|---|
| Realm | `cluster-vending` |
| Issuer | `http://localhost:18080/realms/cluster-vending` |
| API audience | `cv-api` |
| UI client | `cv-ui`, confidential, code flow with S256 PKCE |
| UI callback | `http://localhost:3000/api/auth/callback/keycloak` |
| UI logout return | `http://localhost:3000` or `http://localhost:3000/` |
| CLI client | `cv-cli`, public, code flow with S256 PKCE |
| Optional confidential CLI | `cv-cli-confidential`, code flow with S256 PKCE |
| CLI callback | `http://localhost:18081/callback` |
| Group claim | `groups` (group names without a leading slash) |

The CLI callback must match the CLI's fixed loopback redirect port. Before the
first container start, set `KEYCLOAK_CLI_REDIRECT` if using a different exact
loopback callback. Wildcards and remote callbacks are refused. Imported realms
are deliberately not overwritten on subsequent starts; a changed callback or
client definition requires an explicit development reset below.

## Private files

`bash hack/keycloak.sh prepare` prepares configuration without starting Docker.
It generates random credentials once and reuses them on reruns. Generated files
are ignored by Git under `hack/.keycloak/`, directory mode `0700`, files `0600`:

| File | Purpose |
|---|---|
| `secrets.env` | Keycloak admin/user passwords, both confidential client secrets, session encryption secret |
| `ui.env` | `AUTH_ENABLED=true`, `KEYCLOAK_ISSUER`, `KEYCLOAK_CLIENT_ID`, `KEYCLOAK_CLIENT_SECRET`, `AUTH_SECRET`, `AUTH_URL` |
| `server.yaml` | Copy of the personal server configuration with required auth and `127.0.0.1:8080` binding |
| `container.env`, `import/cluster-vending-realm.json`, `applied-realm.json` | Private container bootstrap material |

The UI development launcher reads `ui.env` only when the shared server config
enables authentication. Explicit server-side environment or `.env.local` client
settings take precedence over generated values; `AUTH_ENABLED` is controlled by
the shared YAML during development. Keep credentials server-side and never use
`NEXT_PUBLIC_` prefixes. Keycloak setup never rewrites `.env.local`.

Local users are `acme-admin` and `acme-viewer`; their passwords are in
`KEYCLOAK_ACME_ADMIN_PASSWORD` and `KEYCLOAK_ACME_VIEWER_PASSWORD` in the private
file. Keycloak administration uses username `admin` and
`KEYCLOAK_ADMIN_PASSWORD`. View credentials privately in your editor when
logging in. No passwords or client secrets are committed or printed by bootstrap.

`KEYCLOAK_BASE_CONFIG` selects the personal server configuration used to generate
the optional `server.yaml` reference copy. Setup preserves the personal config,
credentials and existing realm. It does not enable authentication in either app.
Use the commented auth block in the personal config as the normal opt-in switch.

## Authorization

All three clients get the `cv-api` audience on access tokens and a `groups`
claim. Password grants, implicit flow and service accounts are disabled.
The server uses its existing JWT verifier and authorization rules:

```yaml
auth:
  mode: required
  issuers:
  - name: keycloak
    issuer: http://localhost:18080/realms/cluster-vending
    audiences: [cv-api]
    usernameClaim: preferred_username
    rolesClaim: groups
  rules:
  - match: {issuer: keycloak, role: tenant-acme-admin}
    grant: {role: TenantAdmin, tenants: [acme]}
  - match: {issuer: keycloak, role: tenant-acme-viewer}
    grant: {role: TenantViewer, tenants: [acme]}
```

`acme-admin` belongs only to `tenant-acme-admin`; `acme-viewer` belongs only to
`tenant-acme-viewer`. Neither is a platform administrator. The Keycloak `admin`
is in its management realm and receives no platform API grant. There is no
catch-all grant or self-registration.

CLI login uses the existing browser PKCE flow with issuer above, client ID
`cv-cli`, and the fixed port matching the callback. Then `whoami` should report
one grant for `acme`:

```sh
cluster-vending login --browser \
  --issuer http://localhost:18080/realms/cluster-vending \
  --client-id cv-cli --redirect-host localhost --callback-port 18081
cluster-vending whoami
```

The CLI's default port is ephemeral (`0`), so the explicit `--callback-port`
is required for this realm's exact registered callback. The listener binds
`127.0.0.1`; the redirect URI uses the configured `localhost` host.
The optional confidential client uses
`KEYCLOAK_CLI_CLIENT_SECRET`, supplied through the CLI's client-secret
environment setting; never put the secret on a command line or in kubeconfig.

The CLI defaults to `openid offline_access`. Both local fixture users have
Keycloak's built-in `offline_access` realm role, which authorizes offline refresh
tokens. This role does not add a tenant or platform grant. See
[Keycloak offline access](https://www.keycloak.org/docs/latest/server_admin/#_offline-access).
Without it, browser authentication can return a code but the token exchange is
rejected with `not_allowed`.

Startup recognizes the older fixture's missing offline-role assignments and
adds that role only to `acme-admin` and `acme-viewer`. The migration preserves
credentials, clients, groups, tenant permissions and realm data. Other changes
to the imported fixture still require the explicit development reset procedure.

## Reruns and stopping

`make keycloak` (or `bash hack/keycloak.sh start`) starts/reuses Keycloak only.
`bash hack/keycloak.sh prepare` generates private settings without starting it.
The former `serve` and `run` script actions are removed; use `make run` and
`make run-ui` independently. Stop each foreground application with Ctrl-C.
If an older setup left a background API running, stop that known process before
starting the API; the commands never kill or silently reuse an occupied listener.

Stop Keycloak with `docker stop cluster-vending-keycloak`. `hack/stop.sh`
deletes kind only and leaves the separate Keycloak container and volume intact.

For an intentional development reset, stop Keycloak and remove its container
and volume, then run the bootstrap again:

```sh
docker rm -f cluster-vending-keycloak
docker volume rm cluster-vending-keycloak-data
bash hack/keycloak.sh start
```

This deletes only local Keycloak users/sessions/settings. Keeping `secrets.env`
preserves the generated credentials. If credentials are lost while the Docker
volume remains, restore the private files or perform the explicit reset;
bootstrap will not silently rotate credentials against an existing database.

## UI session security

With authentication enabled, the UI's server handles the confidential-client
code exchange with PKCE, state and nonce. Access/refresh tokens stay inside
authenticated encrypted cookies. HTTPS deployments use host-only `__Host-`
cookies with `Secure`, `HttpOnly`, `SameSite=Lax` and `Path=/`; local HTTP
development omits `Secure` and the `__Host-` prefix. Sessions exceeding one
cookie are split into at most four chunks. Invalid, duplicate, missing,
tampered or expired session cookies do not authenticate a request.

RPC requests must have the configured origin and the session's CSRF token.
The server replaces a browser-supplied Authorization header with the access
token from that verified session and removes browser cookies before forwarding.
The public session response contains the display name and CSRF token, never
OIDC tokens or the client secret. Logout accepts POST with origin/session/CSRF
checks; failed refresh returns an unauthenticated response and clears cookies
rather than forwarding anonymously. Production requires an explicit
`AUTH_ENABLED` setting; `true` requires complete HTTPS issuer/client/session
configuration, while explicit `false` is an intentional opt-out.

The UI deduplicates refreshes inside one Node process and retains completed
results for a two-second grace period for requests carrying the previous
cookie. This is not a distributed lock: another process, replica, or a delayed
request outside that window can refresh the same old token independently.
Concurrent responses can also arrive out of order and replace newer browser
cookies. A strict single-use refresh-token policy can therefore produce a
failed refresh and require sign-in again. Multiple tabs/replicas need an
explicitly compatible IdP reuse policy or coordinated session/refresh handling;
the local passing tests do not establish that guarantee.

The running development realm was inspected through the admin API:
`revokeRefreshToken=false`, `refreshTokenMaxReuse=0`, SSO idle timeout
1,800 seconds and SSO maximum 36,000 seconds. With revocation disabled, this
does not enforce single-use refresh rotation; `refreshTokenMaxReuse=0` should
not be interpreted as a strict replay guarantee on this realm. These are local
development settings, not a production rotation policy. See Keycloak's
[refresh-token rotation documentation](https://www.keycloak.org/docs/latest/server_admin/index.html#_refresh_token_rotation).

The auth tests cover encryption/chunking/cookie ambiguity, CSRF/origin rejection
before provider or upstream requests, session identity forwarding, expired and
failed refresh, callback cleanup, token-free public JSON, POST logout, redirect
validation and production configuration. They also cover refresh deduplication
and immediate late requests inside the process grace window, without claiming
cross-process coordination.

## Deployment outside this laptop

This script and its HTTP issuer are development-only. For a deployed API set
`auth.mode: required` explicitly, use an HTTPS issuer and TLS for the API/UI,
and register exact HTTPS callback URLs. Keep UI authentication enabled and
provide its issuer, client ID, secret and session secret through your secret
manager. Missing auth settings must fail startup rather than fall back to
anonymous access. The existing server's general default is still `none`, so
do not deploy the personal file or omit the required auth section. The local
Keycloak H2 volume is not a production database setup.

The image pin, development container command, realm import behavior and PKCE
settings were checked against the [official Docker guide](https://www.keycloak.org/getting-started/getting-started-docker),
[container guide](https://www.keycloak.org/server/containers),
[realm import guide](https://www.keycloak.org/server/importExport) and
[administration guide](https://www.keycloak.org/docs/latest/server_admin/index.html).

Validation: `go test ./hack/keycloak ./pkg/auth ./pkg/config ./pkg/server/authn`.
After Keycloak starts, `CV_KEYCLOAK_LIVE=1 go test ./hack/keycloak -run TestLiveKeycloakPKCE -v`
exercises real code/PKCE exchanges for both users and all three clients, then
checks signed tokens, tenant grants and denied cross-tenant/viewer-write access.
The live test reads private fixture credentials without printing tokens/passwords.
