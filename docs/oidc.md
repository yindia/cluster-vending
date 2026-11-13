# OIDC sign-in to a cluster's Kubernetes API

A cluster can accept the tokens of an OpenID Connect identity provider next
to IAM: users sign in with their company account and `kubectl` works without
AWS credentials. Microsoft Entra ID (Azure AD) has a preset; any other
provider (Okta, Google, Keycloak, Dex) is configured by its issuer.

This is about the **workload cluster's** Kubernetes API. Signing in to the
cluster-vending API itself is separate.

## What you set

`spec.access.oidc` of a cluster, in one of two forms.

**Microsoft Entra ID.** The tenant and the app registration are all there is
to give:

```yaml
spec:
  access:
    oidc:
      entra:
        tenantId: 833ced3d-cb2e-41de-92f1-29e2af035ddc   # Directory (tenant) ID
        clientId: 00001111-aaaa-2222-bbbb-3333cccc4444   # Application (client) ID
```

The platform fills in the rest:

| Setting | Value | Why |
|---|---|---|
| issuer | `https://login.microsoftonline.com/<tenantId>/v2.0` | the `iss` claim of an ID token from the v2.0 endpoint |
| client | `<clientId>` | the `aud` claim of an ID token is the application ID |
| user name | the `oid` claim, as `entra:<oid>` | the user's object ID: immutable, and the same for every application |
| groups | the `groups` claim, as `entra:<group object id>` | Entra sends group object IDs, not names |

`name` (default `entra`) and `requiredClaims` may be set next to `entra`.
Nothing else may: with `entra`, `issuerUrl`, `clientId` and the claim
settings are refused. To use `upn`, `email` or `preferred_username` as the
user name, use the generic form below with Entra's issuer. Microsoft's own
advice is against it: those values can change and can be reused, `oid` and
`sub` cannot.

**Any other provider.**

```yaml
spec:
  access:
    oidc:
      name: okta
      issuerUrl: https://example.okta.com/oauth2/default   # HTTPS, reachable from the internet
      clientId: kubernetes
      usernameClaim: email      # unset: sub
      usernamePrefix: "oidc:"   # unset: the issuer URL (none when the claim is email); "-" for none
      groupsClaim: groups       # unset: no groups
      groupsPrefix: "oidc:"
      requiredClaims: {hd: example.com}
```

Set, change or remove it on a cluster that exists with the update mask
`spec.access.oidc`:

```sh
cluster-vending cluster update <cluster-id> --tenant acme -f cluster.yaml --update-mask spec.access.oidc
```

## What the platform does

- It renders one EKS identity provider association (an
  `IdentityProviderConfig`) once the EKS cluster is ACTIVE. The cluster
  itself is not touched.
- The association is named `<name>-<hash8 of its settings>`. EKS cannot
  change an association, so a changed provider is a new association under
  a new name; the old one is removed.
- It creates **no RBAC**. A user the provider authenticates can do nothing
  until a RoleBinding or ClusterRoleBinding names the user or one of its
  groups (below). Starter role bindings made by the platform are out of
  scope for now.
- An imported cluster keeps the provider it came with; `spec.access.oidc`
  is refused next to it.

## EKS limits

- **Associating is a cluster update.** The cluster is `UPDATING` meanwhile
  and other updates wait. AWS documents "several minutes"; in practice
  plan for a quarter of an hour or more. Removing is a cluster update too.
- **A change is a remove and an associate.** Between the two, tokens of the
  provider are refused. IAM access keeps working throughout.
- **The issuer must be reachable from the internet** (from the control
  plane's egress path on clusters with customer-routed egress), and its
  certificate cannot be self-signed. Entra qualifies.
- **One provider per cluster on the platform.** EKS itself takes one before
  Kubernetes 1.32 and up to ten after, with unique issuers and names.
- **Prefixes cannot contain `system:`.**

## Microsoft Entra ID: the app registration

1. **Register an application** (single tenant). Its *Application (client)
   ID* and *Directory (tenant) ID* are the two values above.
2. **Authentication → Add a platform → Mobile and desktop applications**,
   with the redirect URIs `http://localhost:8000` and
   `http://localhost:18000` (what `kubectl oidc-login` listens on). No
   client secret is needed: it is a public client with PKCE. Turn on
   *Allow public client flows* if users sign in with a device code.
3. **Token configuration → Add groups claim**, for the ID token. Choose
   **Groups assigned to the application**, then assign the groups that
   should reach clusters to the application (*Enterprise applications →
   Users and groups*).
   - Why not *All groups*: a JWT carries at most **200** groups. A user in
     more gets no `groups` claim at all (a `_claim_names` pointer to
     Microsoft Graph instead), which the Kubernetes API cannot use: the
     user ends up without groups, or is refused.
   - *Groups assigned to the application* needs Entra ID P1, and counts
     direct members only.
   - Groups arrive as **object IDs** (GUIDs). Role bindings name those.
4. **Token version.** `kubectl oidc-login` sends the **ID token**, whose
   version follows the endpoint it was asked from: the issuer above is the
   v2.0 endpoint, so nothing is to set. Only when a client sends the
   **access token** instead (Azure/kubelogin, or `--oidc-use-access-token`)
   must the application issue v2.0 access tokens:
   `api.requestedAccessTokenVersion: 2` in the manifest
   (`accessTokenAcceptedVersion` in the older manifest format). A v1.0
   access token has the issuer `https://sts.windows.net/<tenant>/` and is
   refused.
5. The `oid` claim needs the `profile` scope; the kubeconfig below asks for
   it.

## The kubeconfig

```sh
cluster-vending cluster kubeconfig <cluster-id> --tenant acme --oidc > prod-eks.kubeconfig
KUBECONFIG=prod-eks.kubeconfig kubectl get nodes
```

The file has the cluster's endpoint and CA (from the cluster's recorded
status: the API server has no AWS access) and one user: an exec entry for
[`kubectl oidc-login`](https://github.com/int128/kubelogin)
(`kubectl krew install oidc-login`). It carries **no credential**: the
plugin opens the provider's sign-in page and keeps the tokens on the user's
machine.

```yaml
users:
- name: prod-eks-oidc
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: kubectl
      args:
      - oidc-login
      - get-token
      - --oidc-issuer-url=https://login.microsoftonline.com/833ced3d-cb2e-41de-92f1-29e2af035ddc/v2.0
      - --oidc-client-id=00001111-aaaa-2222-bbbb-3333cccc4444
      - --oidc-extra-scope=profile          # the oid claim needs it
      - --oidc-extra-scope=offline_access   # a refresh token: no browser every hour
      interactiveMode: IfAvailable
```

- Over SSH, add `--grant-type=device-code` to the args.
- For a generic provider the scopes follow the claims: `email` or `profile`
  for the user name claim, `groups` when the groups claim is `groups`. Add
  what your provider needs.
- [Azure/kubelogin](https://github.com/Azure/kubelogin) is an alternative
  for Entra. It sends access tokens: see "Token version" above.
- Without `--oidc` the command is for IAM access: it reads the cluster's
  EKS name and region from the server and runs
  `aws eks update-kubeconfig` with them, adding the cluster to your
  kubeconfig (`--kubeconfig-file` for another file). `--print` prints that
  kubeconfig instead of installing it.

## Giving users permissions

Bind roles to the names the cluster sees: `entra:<user object id>` and
`entra:<group object id>` with the preset, your own prefixes otherwise.

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: entra-platform-admins
subjects:
- apiGroup: rbac.authorization.k8s.io
  kind: Group
  name: entra:0760b6cf-170e-4a14-91b3-4b78e0739963   # the group's object ID
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-admin
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: entra-alice-view
  namespace: web
subjects:
- apiGroup: rbac.authorization.k8s.io
  kind: User
  name: entra:aaaaaaaa-0000-1111-2222-bbbbbbbbbbbb   # the user's object ID
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: view
```

Someone with IAM access to the cluster (an admin principal) applies the
first binding. `kubectl auth whoami` shows the name and groups the cluster
sees for the signed-in user.

## Sources

- AWS, "Grant users access to Kubernetes with an external OIDC provider":
  <https://docs.aws.amazon.com/eks/latest/userguide/authenticate-oidc-identity-provider.html>
  (public issuer, providers per cluster, association as a cluster update,
  prefixes)
- Microsoft, "ID token claims reference":
  <https://learn.microsoft.com/en-us/entra/identity-platform/id-token-claims-reference>
  (`iss` ends in `/v2.0` from the v2.0 endpoint, `aud`, `oid` and the
  `profile` scope, `sub` is per application, mutable claims, the 200-group
  overage)
- Microsoft, "Configure group claims and app roles in tokens":
  <https://learn.microsoft.com/en-us/security/zero-trust/develop/configure-tokens-group-claims-app-roles>
  (group object IDs, the limits, *Groups assigned to the application*, P1,
  direct membership)
- Microsoft, "Access tokens in the Microsoft identity platform":
  <https://learn.microsoft.com/en-us/entra/identity-platform/access-tokens>
  (`requestedAccessTokenVersion`, the v1.0 and v2.0 issuers)
- kubelogin, usage:
  <https://github.com/int128/kubelogin/blob/master/docs/usage.md>
  (flags, redirect URIs, the device code grant)
