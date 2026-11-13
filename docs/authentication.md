# Authentication

The API server can authenticate its callers with OIDC bearer tokens and
authorize each call by tenant and role. Microsoft Entra ID has a preset;
any issuer with an OIDC discovery document and JWT access tokens works
(Keycloak, Okta, Auth0, Dex).

It is off by default: without an `auth` section in the settings file the
server behaves as before and takes the `tenant_id` of a request as given.

Nothing here has run against a real Entra tenant: the server and the CLI
are tested against a fake issuer. The Entra token shapes and endpoints are
from Microsoft's documentation and its live discovery documents (see
[Sources](#sources)).

## Modes

`auth.mode` in the server's settings file (`serve --config`):

| Mode | A request without a token | A request with a token |
|---|---|---|
| `none` (default) | Served. The caller is anonymous and may do everything. | Served; the token is not looked at. |
| `optional` | Served, as with `none`. | The token must verify, and the caller's roles are enforced. A bad token is `UNAUTHENTICATED`. |
| `required` | `UNAUTHENTICATED` (HTTP 401, `WWW-Authenticate: Bearer`). | As `optional`. |

`/healthz`, `/readyz` and `/metrics` are not RPCs and are never
authenticated.

## Configuration

The `auth` section holds lists, so it has no flags and no environment
variables: it is read from the settings file only. It holds no secret:
verifying a token needs the issuer's public keys, nothing else. Unknown
keys are errors, and every problem is reported at startup, together.

```yaml
# hack/config.local.yaml, or the server's ConfigMap
http-addr: ":8080"
auth:
  mode: required
  issuers:
  - name: entra                      # how rules and logs name it
    audiences:                       # a token must carry one as "aud"
    - 99999999-8888-7777-6666-555555555555   # the API's application (client) ID: v2.0 tokens
    - api://cluster-vending                  # its Application ID URI: v1.0 tokens
    entra:
      tenantId: 11111111-2222-3333-4444-555555555555
      acceptV1: true
  rules:
  - {match: {issuer: entra, role: Platform.Admin}, grant: {role: PlatformAdmin}}
  - {match: {issuer: entra, role: Acme.Admin},     grant: {role: TenantAdmin,  tenants: [acme]}}
  - {match: {issuer: entra, role: Acme.Viewer},    grant: {role: TenantViewer, tenants: [acme]}}
```

### Issuers

| Key | Meaning | Default |
|---|---|---|
| `name` | The issuer's name in rules and logs: lower-case letters, digits, dashes. | required |
| `issuer` | The issuer URL: the `iss` of its tokens, and where its discovery document is (`<issuer>/.well-known/openid-configuration`). `https`; `http` is accepted for `localhost` only. | required, unless `entra` is set |
| `audiences` | The `aud` values accepted. A token must carry at least one. | required |
| `entra.tenantId` | Marks a Microsoft Entra ID tenant: the directory (tenant) ID. Sets `issuer` to `https://login.microsoftonline.com/<tenantId>/v2.0`. | |
| `entra.acceptV1` | Also accept the tenant's v1.0 access tokens (issuer `https://sts.windows.net/<tenantId>/`). | `false` |
| `usernameClaim` | The claim shown as the caller's name (`whoami`, the audit log). Not used for authorization. | `sub`; `oid` with `entra` |
| `rolesClaim` | The claim rules match against: a string or a list of strings. | `groups`; `roles` with `entra` |
| `tenantClaim` | A claim whose value is the caller's platform tenant (see below). | none |

### Rules

A rule grants a role to the callers it matches. Every matching rule adds
its grant; there is no first match, and nothing takes access away.

| Key | Meaning |
|---|---|
| `match.issuer` | An issuer's `name`. Required. |
| `match.role` | A value of the issuer's roles claim. Unset: every caller of the issuer. |
| `grant.role` | `PlatformAdmin`, `TenantAdmin` or `TenantViewer`. |
| `grant.tenants` | The tenants a tenant role is for. `["*"]` (alone) is every tenant. Unset for `PlatformAdmin`. |

A caller with no matching rule is authenticated and may only make the
calls open to every accepted caller (`WhoAmI`, the catalog's values file).

**Tenant from a claim.** With `tenantClaim` set on the issuer, a tenant
rule may leave `tenants` out: it then grants its role for the tenant named
in that claim. One rule covers every tenant:

```yaml
  issuers:
  - name: keycloak
    issuer: https://login.example.com/realms/platform
    audiences: [cluster-vending]
    tenantClaim: org            # the token carries {"org": "acme"}
  rules:
  - {match: {issuer: keycloak},                grant: {role: TenantViewer}}   # everyone views their tenant
  - {match: {issuer: keycloak, role: admins},  grant: {role: TenantAdmin}}    # admins administer it
```

A token whose claim is missing, or is not a tenant ID, gets nothing from
such a rule. Only use a claim the issuer controls: a claim users can edit
themselves would let them pick their tenant.

### Roles and what each RPC needs

| Role | May call |
|---|---|
| `PlatformAdmin` | Everything, for every tenant, including `AdminService`. |
| `TenantAdmin` | Every tenant RPC of its tenants. |
| `TenantViewer` | The tenant RPCs marked `NO_SIDE_EFFECTS` (get, list, watch, status) of its tenants. |

The server classifies each RPC from the contract, not from a hand-kept
list: read or write is the method's `idempotency_level` option, and an RPC
whose request has a `tenant_id` is a tenant RPC, checked against that
tenant. The exceptions are listed in `pkg/auth/access.go`:

| RPC | Needs |
|---|---|
| Every RPC with `tenant_id`, marked `NO_SIDE_EFFECTS` | Any role for the tenant |
| Every other RPC with `tenant_id` (create, update, delete, import, takeover, bootstrap, enable, disable) | `TenantAdmin` for the tenant |
| `AdminService.PauseReconcile`, `AdminService.ResumeReconcile` | `PlatformAdmin`, whatever tenant they name |
| `ComponentService.GetComponentValues`, `TenantService.WhoAmI` | Any accepted caller |

An RPC that is neither listed nor carries a `tenant_id` is refused, and a
test (`TestEveryRPCIsClassified`) fails when one is added.

A tenant RPC is checked when the handler receives the request, before it
runs: `PERMISSION_DENIED` names what is missing ("you have no access to
tenant globex", "your access to tenant acme is read-only").

## Microsoft Entra ID

Two app registrations: one for the API, one for the CLI.

### 1. Register the API

In the Microsoft Entra admin center, **Entra ID > App registrations > New
registration**, single tenant. Note the **Application (client) ID** and the
**Directory (tenant) ID**.

- **Expose an API > Application ID URI**: set it. `api://<client-id>` is
  the recommended form.
- **Expose an API > Add a scope**: one delegated scope (for example
  `access`), so users can get a token for the API through the CLI.
- **App roles > Create app role**, one per platform role you want to
  hand out. **Value** is what appears in the token's `roles` claim and
  what `match.role` names; it cannot contain spaces. **Allowed member
  types**: `Users/Groups` for people, `Applications` for automation, or
  both.

  | Value | Allowed member types | Rule |
  |---|---|---|
  | `Platform.Admin` | Both | `grant: {role: PlatformAdmin}` |
  | `Acme.Admin` | Both | `grant: {role: TenantAdmin, tenants: [acme]}` |
  | `Acme.Viewer` | Users/Groups | `grant: {role: TenantViewer, tenants: [acme]}` |

- **Token version.** The manifest's `api.requestedAccessTokenVersion`
  decides what Entra issues for this API: `null` (the default for a new
  registration) and `1` give v1.0 tokens, `2` gives v2.0 tokens. The two
  differ in what the server must be told:

  | | v1.0 | v2.0 |
  |---|---|---|
  | `iss` | `https://sts.windows.net/<tenantId>/` | `https://login.microsoftonline.com/<tenantId>/v2.0` |
  | `aud` | the client ID or the Application ID URI, depending on how the client asked | always the API's client ID |

  Either set `requestedAccessTokenVersion: 2` in **Manifest** and list the
  client ID in `audiences`, or keep the default, set `entra.acceptV1:
  true` and list both the client ID and the Application ID URI. Listing
  both and accepting both, as in the example above, works through the
  change.

### 2. Assign the roles

- **People**: **Entra ID > Enterprise apps > (the API) > Users and groups
  > Add user/group**, pick users or security groups, **Select a role**.
- **Assignment required**: in the enterprise app's **Properties**, set
  **Assignment required?** to **Yes**. Without it, any user or
  application of the directory can get a token for the API. Such a token
  carries no role and so gets no grant, but it is accepted as a caller.

### 3. Register the CLI

A second registration, single tenant, for the public client people log in
with. It has no secret.

- **Authentication > Advanced settings > Allow public client flows**:
  **Yes**. The device code flow needs it.
- For `login --browser`: **Authentication > Add a platform > Mobile and
  desktop applications**, redirect URI `http://localhost/callback`. Entra
  ignores the port of a localhost redirect URI, so the CLI's random port
  matches.
- **API permissions > Add a permission > My APIs > (the API) > Delegated
  permissions**: the scope from step 1.

### 4. Log in

```sh
cluster-vending login \
  --entra-tenant 11111111-2222-3333-4444-555555555555 \
  --entra-client-id <the CLI's client ID> \
  --entra-scope api://<the API's client ID>/.default
cluster-vending whoami
```

`login` prints a URL and a code; open the URL on any device and enter the
code. `whoami` shows the roles in your token and the grants the server's
rules give you.

### 5. Automation

A pipeline is its own app registration with a secret, a certificate or a
federated credential, and gets its token with the client credentials flow.
It does not use `login`.

- In the pipeline's registration: **API permissions > Add a permission >
  My APIs > (the API) > Application permissions**, select the app roles,
  then **Grant admin consent**. The app roles must allow `Applications`.
- Get a token with `scope=api://<the API's client ID>/.default` and
  `grant_type=client_credentials` at
  `https://login.microsoftonline.com/<tenantId>/oauth2/v2.0/token`, and
  pass it in `CV_TOKEN`:

```sh
# The pipeline's own credential; a secret here, kept in the pipeline's secret store.
export CV_TOKEN="$(curl -s -X POST \
  -d "client_id=$PIPELINE_CLIENT_ID" --data-urlencode "client_secret=$PIPELINE_CLIENT_SECRET" \
  --data-urlencode "scope=api://<the API's client ID>/.default" -d grant_type=client_credentials \
  "https://login.microsoftonline.com/<tenantId>/oauth2/v2.0/token" | jq -r .access_token)"
cluster-vending cluster list --tenant acme
```

Such a token has `roles` and no user: `username` is the service
principal's object ID.

### Why app roles, not groups

- The `groups` claim holds group object IDs (GUIDs), not names: rules
  would be unreadable.
- A user in more than 200 groups gets no `groups` claim at all. Entra
  sends an overage claim pointing at Microsoft Graph instead, and the
  server does not call Graph, so that user would lose access.
- App roles belong to the API's registration and arrive in `roles` by
  their value. Assign a security group to an app role to manage
  membership by group.

One limit to know: Entra does not add `roles` for a service principal
that is only a member of a group assigned to the role. Assign app roles to
applications directly (step 5).

Never write a rule without `match.role` for an Entra issuer: it would
grant its role to every user and application of the directory that can get
a token for the API.

## Another issuer (Keycloak)

```yaml
auth:
  mode: required
  issuers:
  - name: keycloak
    issuer: https://login.example.com/realms/platform
    audiences: [cluster-vending]          # the "aud" of the access tokens issued for the API
    usernameClaim: preferred_username
    rolesClaim: groups                    # a claim listing the user's groups or roles by name
  rules:
  - {match: {issuer: keycloak, role: platform-admins}, grant: {role: PlatformAdmin}}
  - {match: {issuer: keycloak, role: acme-admins},     grant: {role: TenantAdmin, tenants: [acme]}}
```

```sh
cluster-vending login --issuer https://login.example.com/realms/platform --client-id cluster-vending-cli
```

The CLI's client is a public client (no secret) that may use the device
authorization grant and, for `--browser`, has the redirect URI
`http://localhost/callback`. The access token must be a JWT whose `aud`
is one of `audiences` and that carries the roles claim; how a client is
made to issue such a token is the issuer's to document, and is not
verified here for Keycloak. For an issuer whose access tokens are opaque or not
meant for other APIs, log in with `--use-id-token` and list the CLI's
client ID in `audiences`: the ID token is then sent.

## The CLI

| Command | What it does |
|---|---|
| `login` | Logs in at the issuer and stores the tokens. Device code flow by default; `--browser` uses the browser of this machine (authorization code with PKCE on a loopback port). |
| `logout` | Removes the stored tokens. |
| `whoami` | Asks the server who you are and what you may do. |

- The tokens are kept in `token.json` in the `cluster-vending` directory of
  your configuration directory (`~/.config` on Linux,
  `~/Library/Application Support` on macOS), mode `0600`. A file other
  users can read is refused.
- Every command that calls the API sends the stored token, renewing it
  with the refresh token when it has expired. Without a login nothing is
  sent; a server in mode `required` then answers how to log in.
- `CV_TOKEN` wins over the stored login.
- A token is never sent over plain `http` to another machine: use an
  `https` `--server`. `http://127.0.0.1` and `http://localhost` are
  allowed, for local testing.

## Rolling it out

1. **`none`**: add the `auth` section with the issuers and rules, keep
   `mode: none`. The configuration is validated at startup; nothing
   changes for callers.
2. **`optional`**: callers that send a token are held to their roles;
   callers that send none still work. Have people `login` and run
   `whoami`; fix the rules until the grants are right. The server logs
   each write with who made it, and `subject=anonymous` shows who has not
   moved yet.
3. **`required`**: once nothing anonymous is left.

The mode is read at startup: changing it is a restart.

## What the server checks

- The token is a JWT signed with a key from the issuer's key set, with
  RS256 or ES256. `none` and the shared-secret algorithms (HS256 and the
  like) are never accepted, whatever the issuer advertises.
- `iss` is a configured issuer, exactly. `aud` contains a configured
  audience. `exp` has not passed (30 seconds of leeway); `nbf`, when
  present, has (5 minutes of leeway).
- The issuer's discovery document and keys are read on the first token
  that names it, not at startup, and the keys are fetched again when a
  token is signed with one the server has not seen. While an issuer cannot
  be reached its tokens are refused with `UNAVAILABLE`; the failure is
  remembered for 15 seconds, so an outage does not turn every request into
  a call to the issuer. The server starts and is ready without it.
- Every write, and every refused call, is logged with the procedure, the
  subject, the username, the issuer, the tenant and the outcome. Tokens are
  never logged.

## What this does not do

- **`none` is no protection.** Anyone who reaches the API can act for any
  tenant. That is today's default, for local testing; do not expose such a
  server.
- **`optional` protects only callers that send a token.** A caller that
  sends none is still anonymous and may do everything. It is a migration
  step, not a security setting.
- **No revocation.** A token is accepted until it expires, also after the
  user lost the role or was disabled. Keep access token lifetimes short.
- **No TLS.** The server speaks plain HTTP; put it behind something that
  terminates TLS. A bearer token sent over plain HTTP can be read and
  replayed.
- **`/metrics` is open.**
- **The controller's kinds are not covered.** Whoever can write
  `Cluster` or `Network` objects in the management cluster with `kubectl`
  bypasses the API; that is Kubernetes RBAC's to restrict.
- **No per-resource permissions.** A role is for a whole tenant.

## Sources

Checked on 2026-11-13.

- Access token claims, v1.0 and v2.0 (`iss`, `aud`, `roles`, `groups`,
  the groups overage at 200):
  <https://learn.microsoft.com/en-us/entra/identity-platform/access-token-claims-reference>
- `requestedAccessTokenVersion` (null defaults to 1), `groupMembershipClaims`,
  Application ID URI formats:
  <https://learn.microsoft.com/en-us/entra/identity-platform/reference-microsoft-graph-app-manifest>
- App roles: creating, assigning to users, groups and applications, admin
  consent, app roles compared with groups:
  <https://learn.microsoft.com/en-us/entra/identity-platform/howto-add-app-roles-in-apps>
- Device authorization grant (endpoints, parameters, polling errors):
  <https://learn.microsoft.com/en-us/entra/identity-platform/v2-oauth2-device-code>
- Client credentials flow (`/.default` scope, tokens without `roles`,
  assignment required):
  <https://learn.microsoft.com/en-us/entra/identity-platform/v2-oauth2-client-creds-grant-flow>
- Application ID URI and scopes of an API (Expose an API):
  <https://learn.microsoft.com/en-us/entra/identity-platform/quickstart-configure-app-expose-web-apis>
- Public client flows and desktop redirect URIs:
  <https://learn.microsoft.com/en-us/entra/identity-platform/scenario-desktop-app-configuration>
- Localhost redirect URIs (port ignored, `http` allowed):
  <https://learn.microsoft.com/en-us/entra/identity-platform/reply-url>
- The live discovery documents, for the two issuer values and their
  endpoints:
  `https://login.microsoftonline.com/common/.well-known/openid-configuration`
  and `https://login.microsoftonline.com/common/v2.0/.well-known/openid-configuration`
