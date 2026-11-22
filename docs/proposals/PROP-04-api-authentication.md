# PROP-04: API authentication only, no roles

| | |
|---|---|
| Status | Accepted (2026-11-22), implementation in progress |
| Author | `<author>` |
| Date | 2026-11-22 |
| Reviewers | `<reviewers>` |
| Related | [architecture.md](../architecture.md): Authentication, 7. Known limits; [PROP-02](PROP-02-cluster-data-live-and-snapshot.md) 7.5 (live reads without auth) |

## 1. Summary

Decision (user, 2026-11-22): "remove rbac from the api, cli and server, in server just need auth from OIDC".

- The API server authenticates callers with OIDC and does nothing else: a token that an issuer in the
  configuration signed, for this API's audience, not expired. That is the whole check.
- There is no authorization. The `PlatformAdmin`, `TenantAdmin` and `TenantViewer` roles, the
  `auth.rules` that grant them, the roles and tenant claims, the read-only level and every per-tenant
  check against the caller are removed. An authenticated caller may call every RPC, the platform-wide
  ones included (platform freeze, releases, channels, tenant creation), for every tenant.
- The CLI and the console drop every role: no grants in `whoami`, no hidden or disabled buttons, no
  read-only notices. Who is signed in is still shown.
- `auth.mode` keeps two values: `required` (OIDC) and `none` (local development, no token read, a warning
  at start). `optional` is removed.

## 2. How it works today

Checked against `main`.

- **Configuration** (`pkg/auth/config.go`): `auth.mode` is `none` (the default),
  `optional` or `required`. `auth.issuers` lists OIDC issuers (`issuer`, `audiences`,
  `usernameClaim`, `rolesClaim`, `tenantClaim`, an `entra` preset). `auth.rules` map a
  value of the roles claim to a grant: a role (`PlatformAdmin`, `TenantAdmin`,
  `TenantViewer`) for a list of tenants, `*`, or the tenant in `tenantClaim`.
- **Verification** (`pkg/auth/verify.go`): go-oidc with discovery and JWKS; RS256 and
  ES256; `iss`, `aud` and `exp` are checked. The resulting `Identity` carries subject,
  username, issuer name, roles and grants (`pkg/auth/identity.go`).
- **Interceptor** (`pkg/server/authn/authn.go`): mode `none` serves every request as
  anonymous. `optional` lets requests without a token through as anonymous.
  `required` refuses them with `UNAUTHENTICATED` and `WWW-Authenticate: Bearer`. A
  sent token must verify in both modes.
- **Classification** (`pkg/auth/access.go`): an RPC whose request has `tenant_id` is a
  tenant RPC. Nine RPCs are listed by name: the six `AdminService` RPCs are
  `ScopePlatform`; `GetComponentValues`, `WhoAmI` and `ListTenants` are
  `ScopeAuthenticated`. A method not marked `NO_SIDE_EFFECTS` is a write. An RPC that
  is neither listed nor has `tenant_id` is refused, and a test fails when one is added.
- **Authorization** (`Identity.Authorize`): an anonymous caller or a `PlatformAdmin` may
  do everything. A tenant RPC needs a `TenantAdmin` grant for the request's tenant, or
  `TenantViewer` for a read. Handlers call it again where the request shape needs it
  (`pkg/server/tenant/tenant.go`, `dashboard.go`, `directory.go` for `CreateTenant` and
  the `ListTenants` filter, `pkg/server/subtenant/service.go`).
- **Who may create a tenant** (`directoryCanCreate`, `pkg/server/tenant/directory.go`):
  anonymous, a `PlatformAdmin`, or any caller with a `TenantAdmin` grant.
- **WhoAmI** (`pkg/server/tenant/whoami.go`, `idl/cloud/v1/tenant_service.proto`):
  returns `anonymous`, `subject`, `username`, `issuer`, `roles` and `grants`
  (`AccessGrant` with the `AccessRole` enum).
- **Audit**: the interceptor logs one line per write and per refused call, with
  procedure, subject, username, issuer, tenant and outcome. `pkg/server/cluster/retry.go`
  and `pkg/server/admin/admin.go` record the caller's username or subject on what they do.
- **UI**: a Next.js server holds the session in an encrypted cookie and forwards the
  user's access token to the API (`ui/src/lib/auth/server.ts`, `ui/src/lib/api/proxy.ts`).
  Login is OIDC code flow with refresh, enabled by `AUTH_ENABLED`, `KEYCLOAK_*`,
  `AUTH_SECRET` and `AUTH_URL`. `sessionAccess` (`ui/src/lib/api/access.ts`) derives
  `isAdmin` and `canWrite` from `WhoAmI` grants and hides buttons with them in about 15
  components (freeze, releases, accounts, networks, clusters, components, sub-tenants).
  architecture.md still says web login is not implemented; that sentence is out of date.
- **CLI** (`pkg/cli/login`, `cmd/auth.go`): `login` with the device grant or the browser
  with PKCE, the token kept in a local file and refreshed; `CV_TOKEN` wins for automation.
- **Local Keycloak** (`hack/keycloak/main.go`, `hack/keycloak.sh`): realm
  `cluster-vending`, clients `cv-ui`, `cv-cli`, `cv-cli-confidential`, audience `cv-api`,
  a `groups` claim; groups `tenant-acme-admin` and `tenant-acme-viewer` with one user
  each; the two rules of `hack/config.local.yaml` (commented out) map them to
  `TenantAdmin` and `TenantViewer` on `acme`.
- **Deployments**: `config/server/base` runs mode `none`. `config/server/auth` is the
  opt-in example with mode `required` and the two rules.
- **Known limit** (architecture.md 7): with `auth.mode: none` the server takes the
  `tenant_id` of a request as given. PROP-02 7.5 refuses live reads in that mode unless
  `live.allowWithoutAuth` is set.
- **Rate limiting**: the API has none today (only the controller's watch has a limiter).

## 3. Problems

| # | Problem | Where |
|---|---|---|
| P1 | Roles are a second permission system to keep in step with the identity provider: rules, a roles claim, three roles, a viewer level per RPC, and grants the UI copies to hide buttons. The request is authentication only. | `auth.rules`, `Identity.Authorize`, `sessionAccess` |
| P2 | `optional` is a third mode that serves callers without a token. | `ModeOptional` |
| P3 | WhoAmI, the CLI and the console carry role data nobody needs once roles go. | `WhoAmIResponse.roles`, `grants` |

## 4. Goals and non-goals

### Goals

1. In `required` mode, every RPC needs a verified token: signature from the issuer's JWKS, `iss`, `aud`,
   `exp` (and `nbf` when present). Health, readiness and metrics stay open.
2. No roles, no rules, no read-only level, no tenant membership check: any authenticated caller may do
   everything.
3. No role data in the API (WhoAmI), the CLI or the console.
4. Existing config files keep loading: old role keys warn, they do not fail.
5. Every call keeps its subject in the audit log.

### Non-goals

- Authorization of any kind (per tenant, per verb, per resource). If it is needed later, it is a new
  proposal.
- A user store, sessions or API keys in the server. Identity stays with the issuer.
- Changing token verification (algorithms, discovery, the Entra preset).
- Kubernetes RBAC in workload clusters, and the workload-cluster identities of PROP-02 (the live reader's
  ClusterRole is about what a process may read in a cluster, not about API users).
- Rate limiting: none today, none added here.

## 5. Design

### 5.1 Modes

| Mode | Behaviour |
|---|---|
| `required` | Every RPC needs a token that verifies. A request without one, with a bad signature, the wrong audience or an expired token is `UNAUTHENTICATED` with `WWW-Authenticate: Bearer`. Needs at least one issuer. |
| `none` | No token is read. Every caller has full access. The server logs one warning at start saying so. For local development (`hack/config.local.yaml`). |

`optional` is refused at start with "auth.mode optional is removed: use required, or none for local
development".

### 5.2 Verification

Unchanged: go-oidc with discovery and JWKS, RS256 and ES256, `iss`, `aud` and `exp` checked, the Entra
preset, the username claim. The identity kept for a request is subject, username and issuer: used for the
audit log and for the "requested by" fields that `retry` and the admin RPCs record.

### 5.3 What an authenticated caller may do

Everything. Tenant RPCs take the request's `tenant_id` as given (it must still exist and be a DNS label:
that is request validation, not authorization). Platform RPCs (`AdminService`) and `CreateTenant` are open
to every authenticated caller. `ListTenants` lists every tenant.

Consequence, stated plainly: everyone who can obtain a token from a configured issuer for this audience can
read and change every tenant and freeze the platform. Access is decided at the identity provider: who can get
a token for the `cv-api` audience (for Keycloak, which users can use the `cv-ui` and `cv-cli` clients; for
Entra, app assignment required on the API's enterprise application).

### 5.4 Callers other than people

- The console's server forwards the signed-in user's access token, as today.
- The CLI sends its login token, or `CV_TOKEN` for automation (a client-credentials token from the same
  issuer).
- The controller is not an API caller.
- The live reader of PROP-02 is called only by the API (NetworkPolicy, ServiceAccount check); it is not an
  API user.

### 5.5 Migration of existing configuration

| Key | Behaviour |
|---|---|
| `auth.mode: optional` | Refused at start (5.1). |
| `auth.rules` | Ignored, one warning: "auth.rules are ignored: the API has no roles". |
| `auth.issuers[].rolesClaim`, `tenantClaim` | Ignored, one warning each. |
| Everything else in `auth` | Unchanged. |

### 5.6 What is removed

| Where | What goes |
|---|---|
| `pkg/auth` | `Role` and its values, `AllTenants`, `Rule`, `Match`, `Grant`, `Config.Rules`, `RolesClaim`, `TenantClaim`, `ModeOptional`, `Identity.Roles`, `Identity.Grants`, `Authorize` and its read-only branch, `IsPlatformAdmin`, `grantsFor`, the access scope check. |
| `pkg/server` | Every handler-level authorization call (tenant, dashboard, directory, subtenant services), `directoryCanCreate`'s role rule, the `ListTenants` filter by grant, the `optional` branch of the interceptor. |
| `idl/cloud/v1/tenant_service.proto` | `AccessRole`, `AccessGrant`, `WhoAmIResponse.roles` and `grants`: field numbers and names reserved. |
| CLI | Role and grant output of `whoami`, role hints in errors. |
| Console | `canWrite`, `isAdmin` and their callers; read-only notices; role-gated pages and menus. |
| `hack/keycloak`, `hack/config.local.yaml`, `config/server/auth` | The `tenant-acme-admin` and `tenant-acme-viewer` groups and users, and the rules. One demo user stays. |
| `docs/architecture.md` | The roles table, rules, the `optional` mode and the tenant-authorisation known limit. |

**Stays:** issuers and verification, audiences, the username claim, the CLI login (device grant, browser
with PKCE, refresh), the console session and token forwarding, the audit log, the open health and metrics
endpoints.

## 6. Security

- **Who gets in is decided at the issuer.** With no authorization in the API, the audience and the
  issuer's assignment rules are the only gate (5.3). The deployment guide says to restrict who can obtain a
  token for the API's audience.
- **`none` is full access for anyone who reaches the port.** It is for a laptop. The warning at start says
  so. `config/server/base` should run `required` in any shared environment.
- **No tenant isolation in the API.** Customers that must not see each other need separate installations,
  or a later authorization proposal.
- **Logs.** No token is ever logged; subjects and usernames are, on every write and every refusal.

## 7. Alternatives considered

| Alternative | Why not |
|---|---|
| Keep tenant membership from a token claim | It is authorization; the decision is authentication only. |
| An operator list for platform RPCs | Same: authorization. Restrict token issuance instead. |
| Keep the viewer level | Same. |
| Keep `optional` | With no roles it only means "tokens are optional": `none` covers local use, `required` the rest. |
| API keys in the server | A credential store, rotation and revocation the issuer already provides (client credentials). |

## 8. Rollout plan

One change, since roles are removed everywhere at once.

### Phase 1: Remove roles, keep OIDC

- Everything in 5.6. The proto reserve and the regenerated Go and TypeScript. Old keys warn (5.5).
- **Accepted when:** with `required`, a valid token calls a platform RPC and a tenant RPC of any tenant;
  no token, a bad signature, the wrong audience or an expired token is `UNAUTHENTICATED`; `optional` stops
  the server at start; a file with `auth.rules` loads with a warning; `whoami` shows the identity only; the
  console shows every page and action to a signed-in user.
- **Tests:** the interceptor table (valid, missing, bad signature, wrong audience, expired), the config
  loader (optional refused, old keys warn), WhoAmI, the CLI `whoami` output, the console unit and browser
  tests without role fixtures.

### Phase 2: Deployment defaults (follow-up)

- `config/server/base` runs `required` with a placeholder issuer, and the local files keep `none`.
- **Accepted when:** the base overlay refuses to start without an issuer, and `make run` still runs with
  `none`.

## 9. Open questions

None. Restoring any authorization later (tenant isolation, an operator list, a read-only level) is a new
proposal.
