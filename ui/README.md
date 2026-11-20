# Cluster Vending UI

Next.js App Router console, in React and TypeScript with the Astryx Neutral design
system. It reads the platform through the Go Connect API and keeps no state of its
own: no database, no AWS access. The tenant console is at
[localhost:3000](http://localhost:3000/) when run locally.

The console shows tenants, accounts, networks, clusters, components, releases and
channels, and tenant freezes. Create and import use guided forms or CLI-compatible
YAML (`cluster-vending.io/v1` manifests), with a review step before submission.
Destructive actions need explicit confirmation. The console reads only what the
caller's grants allow. The API checks every change again.

What the console does with the platform is described in
[docs/architecture.md](../docs/architecture.md); the topology views read the
API's `architecture` projection.

## Development

Use Node 24 or newer (below 27), then install the locked dependencies:

```sh
npm ci
npm run dev
```

Development reads the API's `auth` section in `../hack/config.local.yaml`. With that
section commented out, login is off. To enable local login, see
[local login](../docs/architecture.md#local-login). Set the server-only
`CV_API_BASE_URL` to the Go API's URL (default `http://127.0.0.1:8080`).

Use an already-running UI server when one exists; do not start a second listener.

## Checks

```sh
npm test
npm run lint
npm run typecheck
npm run build
npm run doctor
npm run test:browser
```

Unit tests mock the API. Browser tests use the running server on port 3000 and
intercept every RPC with fixtures, so they change no account or backend. Set
`CV_UI_TEST_BASE_URL` to test another target. The production dependency audit is
`npm audit --omit=dev`. Generated code in `src/gen` is excluded from lint.

## Generated API contracts

```sh
npm run codegen
```

This runs `buf generate --template ui/buf.gen.yaml` from the repository root. The
generated files in `src/gen` are required by the runtime clients and are committed.
Regenerate them after changing the contracts in `idl/`.

## Design system

Astryx agent conventions are in [AGENTS.md](AGENTS.md). The CLI owns
`src/astryx-themes.ts`. Extend the imported theme with `defineTheme({ extends: ... })`;
do not eject it.

## Production

`npm run build` prepares Next's standalone server. Run `npm start` only when a server
is not already running. Set `PORT` and `HOSTNAME` as the deployment needs. A nonroot,
multistage [Dockerfile](Dockerfile) is supplied; build its context from `ui/`.

The console takes an operator-supplied access token; web OIDC login and token refresh
are not implemented. Deploy it behind an access boundary, and require authentication
in the Go API. Never embed a shared administrative token in a browser bundle. GCP and
Azure are not usable backends.
