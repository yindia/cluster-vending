# Cluster Vending UI

Next.js App Router application using React, TypeScript and Astryx Neutral.

The console at [localhost:3000](http://localhost:3000/) reads live inventory through
the existing Go Connect API. Accounts, networks and clusters have dedicated detail
URLs, resource inventories and reviewed create/edit workflows. Create and edit
forms use modal steps with a final review; destructive actions require explicit
confirmation. Releases, channels, component catalog and scoped freeze controls
use the generated contracts. Components, releases and channels also have dedicated details. Helm values use
YAML/JSON code editors with syntax highlighting, line numbers and copy controls.
The static design preview remains at `/preview`.

Select a tenant from the searchable **Tenant** control in the top bar. Open
**Settings** to supply your own access token if required.
The token stays in tab memory. Background refresh is **off by default**; use the
page Refresh actions or enable Live updates in Settings. Submitted operations
are observed until completion with bounded polling. Set server-only `CV_API_BASE_URL` to the Go API URL
(default `http://127.0.0.1:8080`). No database is added.

## Development

Use Node 24 or newer (below 27), then install the locked dependencies:

```sh
npm ci
npm run dev
```

Use an already running UI server when one exists; do not start a second listener.
From the repository root, this application is in `ui/`.

## Checks

```sh
npm test
npm run lint
npm run typecheck
npm run build
npm run doctor
npm run test:browser
```

Unit tests mock the API. Browser tests use the already-running server at port 3000
and intercept every RPC with fixtures; they do not mutate a cloud account or backend.
Override the browser target with `CV_UI_TEST_BASE_URL` when needed.
The production dependency audit is `npm audit --omit=dev`.
Generated source in `src/gen` is excluded from lint because it is compiler-owned.

Account, network and cluster lists use single-line table rows with separate ID
columns, search and filters. Network and cluster lists exhaust API pagination
before filtering locally; list search includes resources beyond the visible page.
Detail resource inventories support search and Kind/status/pause filters. Overview,
Activity, deprecation findings and workload recommendations also expose Kind filters.
Event search and filters inspect all available event pages. Event messages reveal
full text on hover, focus or touch; no separate full-message action is required.
All console tables support pointer and keyboard column resizing, with a reset
action to restore default widths. There is no separate width configuration menu.
Wide tables scroll horizontally. Complete detail tables offer local search;
paginated inventories filter complete loaded datasets before slicing pages.
Activity, event lists and release history use their existing tables and pagination
without date inputs. Time entry is reserved for an optional freeze expiry.
Account, network and cluster detail headers offer **View YAML**: a read-only,
copyable configuration snapshot. Networks and clusters request the original input;
if unavailable, the dialog identifies the returned configuration as resolved.
Observed status and API etags are omitted.
Cluster details also expose **Deprecated APIs** and **Recommendations** as
read-only, tenant-scoped views of the controller's recorded results. Refresh reads
stored findings; it does not trigger a scan. Collection timestamps, errors,
coverage and missing-data states distinguish an old or absent result from a
successful check.

The cluster's Components tab includes the tenant catalog, so available components
can be enabled and configured there through the shared YAML review/apply workflow.
Components disabled by platform policy remain unavailable; platform-managed
components remain read-only.

This dashboard is tenant-only. Tenant viewers can inspect releases and freeze
status; tenant administrators can freeze and unfreeze their own tenant, networks
and clusters. The API verifies resource ownership before changing policy. Platform
administration RPCs are unavailable through the dashboard proxy. An active platform
maintenance freeze is a read-only restriction that tenant users cannot lift.

Freeze controls expose an explicit **Unfreeze scope** action for tenant
administrators. It remains available while a freeze is converging, with canonical
status and typed scope confirmation still required. Cluster and network detail
actions open their own preselected freeze scope; opening that page only reads
status and never changes policy automatically.

Release-channel details include a **Clusters** tab for the selected tenant. It
loads all cluster pages before filtering by channel, includes explicit channel
selections and the automatic Stable default, and shows upgrade mode separately.
Manual clusters with no channel are not subscribers. The table is independent of
current rollout membership and supports search, filters and cluster detail links.

The top bar lists only registered tenants the caller may read. **Create tenant**
is available when the API and the caller’s grants permit the requested ID. It
creates a cluster-scoped Tenant CR and its Kubernetes namespace, then selects the canonical returned tenant; it
does not create a cloud account or change access grants. Switching tenants leaves
resource detail context and cancels the old scope’s reads and drafts.

Create dialogs offer a guided form or **Paste YAML**, using the CLI's
`cluster-vending.io/v1` manifests (`CloudAccount`, `Network`, `Cluster`). YAML is
bounded to 64 KiB and checked against generated protobuf validation rules. Review
performs read-only reference checks; server admission occurs on final submission.
Cloud-account creation requires an existing Tenant CR and its namespace; it never
creates either implicitly. Tenant registration owns namespace setup and the tenant
controller maintains it. Effective freeze policy is reflected on the Tenant CR.

The reviewed request preserves advanced spec fields and optional values. Tenant
mismatches and create-time IDs are rejected. Cluster tier comes from its network.

Import dialogs likewise accept guided input or CLI-compatible **Paste YAML**
(`NetworkImport`, `ClusterImport`). Validation and reference checks only read;
final confirmation records observe-only discovery. Explicit **Take over** actions
in imported network and cluster headers review the plan before requesting
ownership. Cluster takeover re-reads the cluster and parent network before submit;
changed etags or plans require fresh consent. NOT_IMPORTED resources require
acknowledgement, and cluster takeover offers the persistent skip-platform-components
choice.

Cluster details offer **Get kubeconfig** to copy local CLI install/print commands,
with OIDC commands only when configured. Commands use positional cluster IDs and
run with the user’s own credentials; the browser does not execute them.

## Generated API contracts

```sh
npm run codegen
```

This calls `buf generate --template ui/buf.gen.yaml` from the repository root.
The root `buf.gen.yaml` generates both Go and TypeScript. The UI-only template
avoids rewriting the existing generated Go tree. Protobuf-ES generates message
schemas and service descriptors; Connect clients consume those descriptors.

The generated files in `src/gen` are required by the runtime clients and manifest
validation; keep them committed. Regenerate them after changing the contracts.

Local reference material in `docs/`, dependency directories, build output, test
reports, environment files and generated tool pointers are excluded by `.gitignore`.

## Design system

Astryx agent conventions are in [AGENTS.md](AGENTS.md). The CLI owns
`src/astryx-themes.ts`; `src/app/providers.tsx` imports `themes` and
`defaultThemeSlug` once and passes `themes[defaultThemeSlug]` to `Theme`.
Use the installed components and prebuilt stylesheets. No StyleX compiler is
needed until custom StyleX source is introduced.

For customization, extend the imported theme with
`defineTheme({ extends: importedTheme, ... })` and build static theme assets.
Do not eject a theme unless an independent source fork is explicitly requested.
Neutral's font stylesheet currently uses Google Fonts for Figtree, with the
theme's fallback font stack if it cannot load.

## Production

`npm run build` prepares Next's standalone server with its static/public assets.
Run `npm start` only when a server is not already running. Set `PORT` and
`HOSTNAME` in the deployment environment as needed. A nonroot, multistage
[Dockerfile](Dockerfile) is supplied; build its context from `ui/`.

The current connection flow accepts an operator-supplied access token; web OIDC
login and refresh are not implemented. Backend authorization remains authoritative.
Deploy behind the appropriate access boundary and require authentication in the Go
API. Never embed a shared administrative token in a browser bundle. Kubernetes remains the source
of truth; the UI adds no database. GCP and Azure are planned/disabled, not usable
backend providers.
