# Sub Tenant OIDC access and end-user kubeconfig

Status: proposed plan only. No API, controller, UI, identity-provider or workload resources are changed by this document.

## Recommended behavior

A tenant administrator supplies the IdP groups allowed to use a Sub Tenant's namespaces. End users download a credential-free kubeconfig, run kubectl, and sign in through their organization's IdP. Kubernetes grants access using the groups in the validated token and namespace-scoped RoleBindings.

Authentication belongs to the cluster's external OIDC integration. Authorization belongs to the Sub Tenant's approved group bindings. The kubeconfig's default namespace is a convenience, not an access boundary.

Use the existing Capsule Tenant and shared GlobalTenantResources. Add structured access bindings to the management SubTenant, extend the trusted translation into Capsule Tenant data, and generate one stable RoleBinding per binding per selected namespace. Do not add human groups as Capsule owners merely to grant workload access.

## Existing foundation and gaps

- `spec.access.oidc` already configures an external EKS identity-provider association; `pkg/oidc.Resolve` handles generic issuers and the Entra preset.
- `pkg/cli/kubeconfig.go` already supports `cluster kubeconfig --oidc`, with an exec plugin invoking `kubectl oidc-login`. Reuse this functionality by extracting a shared pure builder, rather than writing a second generator or importing the CLI into the server.
- The dashboard's Get kubeconfig workflow currently produces an AWS IAM command, not an external OIDC download.
- Current `SubTenant.spec.access.bindings` accepts opaque `identityRef` plus `workload-editor`. Nonempty bindings are rejected because no trusted resolver exists. Raw group names are not supported today.
- The access GTR currently expects one `developerGroup`. It must become a validated binding list with namespace scope, stable names and observable revocation.
- Identity resolution and runtime isolation qualification are not implemented. The current access gate remains false; adding an OIDC kubeconfig alone must not open it.
- Live namespace observation currently miscompares a default-deny NetworkPolicy containing explicit empty rule lists with Kubernetes' stored form omitting them. Fix semantic normalization while preserving real drift detection before claiming baseline completion. Provisioning completion and access qualification also need separate, actionable conditions instead of an unexplained perpetual Provisioning state.

## 1. Distinguish user OIDC from workload IAM identity

`spec.aws.irsa: true` enables workload service-account federation. It does not enable human OIDC sign-in. For this flow require `spec.access.oidc`, a configured groups claim, and an observed active EKS identity-provider association matching the effective issuer/audience/claim settings. Do not use the EKS service-account issuer as the human login issuer.

EKS accepts the external provider's ID tokens according to its configured issuer, audience and claim mapping. Prefer an explicit nonreserved groups prefix to keep identity domains distinct. [AWS external OIDC documentation](https://docs.aws.amazon.com/eks/latest/userguide/authenticate-oidc-identity-provider.html)

Represent login-client configuration separately where EKS authentication settings do not express it: approved public-client scopes, redirect URI profile and supported login flows. Do not infer every provider's required scopes solely from the claim name. Resolve this centrally with `pkg/oidc`, shared by the association adapter, download builder and authorization translator.

## 2. Proposed Sub Tenant access contract

Illustrative UI/API YAML changes, not a currently accepted manifest:

```yaml
spec:
  namespaceKeys: [app, worker]
  access:
    bindings:
      - group: payments-developers
        roleProfile: workload-editor
        namespaceKeys: [app]
      - group: payments-observers
        roleProfile: workload-viewer
        namespaceKeys: [app, worker]
```

The group is the exact **unprefixed token-claim value**, not a display label, glob or Kubernetes subject. With `groupsPrefix: "oidc:"`, the first effective subject is `oidc:payments-developers`. For the existing Entra preset, users supply group object IDs and the platform derives `entra:<group-object-id>`. Directory membership and group claims must actually be configured at the IdP; typing a name into this YAML creates neither a directory group nor membership.

Contract decisions:

- Initially use the cluster's single effective OIDC provider. Bind resolution to its immutable effective identity/settings revision; future multiple issuers require an explicit provider reference.
- `group` and legacy `identityRef` are mutually exclusive. Preserve existing protobuf field numbers; add a discriminated principal without reusing fields or interpreting old opaque references as group names. Legacy references remain rejected until a resolver supports them.
- `namespaceKeys` may contain only existing requested keys on this allocation. Omission or an empty list means all current and future keys of this Sub Tenant; a nonempty list is a fixed subset. Show this difference in review so adding a namespace cannot unexpectedly widen a fixed binding.
- Start with fixed `workload-editor` and a newly reviewed `workload-viewer` profile. Do not accept arbitrary Role/ClusterRole names, wildcard permissions, raw User subjects or ServiceAccount subjects through this interface.
- Deduplicate and sort bindings deterministically. Enforce bounded counts and string lengths; reject empty groups, wildcards, reserved system identities, double-prefix ambiguity and unknown namespace keys.
- Only tenant administrators with access-management permission can change bindings. A group's presence in a request is not proof that the caller can delegate it: require an issuer-bound tenant group allowlist or trusted directory ownership resolver. Store the approved delegation policy on the management Tenant/configuration initially; a separate group CR is unnecessary for the first version.
- Match groups exactly, with the IdP's case-sensitive semantics. Do not lowercase, substring-match or add prefixes supplied by the caller. Preserve original intent separately from canonical effective subjects in status.

## 3. Resources and ownership

| Location | Resource/change | Owner and purpose |
| --- | --- | --- |
| Management | Existing Tenant delegation policy | Trusted policy identifies groups this platform tenant may delegate; no database |
| Management | Existing SubTenant access spec/status | Desired bindings, immutable issuer identity, observed binding revision and namespace scope |
| Workload, once per cluster | Existing EKS OIDC association | Platform cluster controller; association must be observed active |
| Workload, once per cluster | Existing `naas-workload-editor`, new narrow viewer ClusterRole | Bootstrap release; reusable permission definitions |
| Workload, once per cluster | Updated `naas-developer-access` GTR and constrained replication RBAC | Bootstrap release; generates/removes only approved bindings |
| Workload, per allocation | Existing Capsule Tenant | Management SubTenant controller updates approved data; trusted provisioner remains owner |
| Workload, per binding/namespace | RoleBinding | Capsule GTR replicates approved exact Group subject to the fixed ClusterRole |

A RoleBinding can reference a shared ClusterRole while granting its permissions only in the binding's namespace. Kubernetes RBAC permissions are additive: a separate broad binding can still grant wider access, so this design assumes the user has no unrelated administrative grant. [Kubernetes RBAC documentation](https://kubernetes.io/docs/reference/access-authn-authz/rbac/)

Illustrative generated binding:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: naas-access-<stable-binding-id>
  namespace: st-<subtenant-uuid-without-hyphens>-app
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: naas-workload-editor
subjects:
  - apiGroup: rbac.authorization.k8s.io
    kind: Group
    name: oidc:payments-developers
```

No per-user Kubernetes User/Group objects, long-lived ServiceAccount tokens, client certificates, EKS IAM access entries or tenant ClusterRoleBindings are needed for this OIDC path. Do not create TenantOwner resources for developer groups: Capsule ownership carries namespace-management and other owner capabilities beyond ordinary application access. [Capsule permissions](https://projectcapsule.dev/docs/tenants/permissions/)

Keep native namespace creation under `namespaceKeys`, not arbitrary developer `kubectl create namespace`. Default profiles do not grant cluster-wide namespace enumeration. A user can work with `kubectl -n <allowed-namespace> get pods` even when `kubectl get namespaces` is forbidden. Namespace discovery UX, if needed later, is a separate narrowly scoped API/proxy feature.

The existing editor can create workloads; admission must prevent selecting privileged service accounts, requesting elevated Pod identities, or changing protected enrollment/policy metadata. Narrow namespace RBAC is not sufficient to qualify workload isolation by itself.

## 4. End-user login flow and kubeconfig

```text
Dashboard / CLI -> authenticated metadata-only kubeconfig download
kubectl -> local oidc-login exec plugin -> browser -> organization IdP / MFA
IdP -> ID token -> local plugin -> Kubernetes API
EKS validates issuer/audience/claims -> RBAC matches group -> selected namespace
```

Use a public OAuth client with authorization-code login and PKCE S256. Offer device-code login only when the provider/client supports it. Ask the client owner to register the supported loopback redirects; no confidential client secret belongs in a downloadable file. The plugin supports PKCE, provider-specific scopes, local caching and an OS-keyring option. [kubelogin usage](https://raw.githubusercontent.com/int128/kubelogin/master/docs/usage.md)

Illustrative file, using approved generic IdP settings:

```yaml
apiVersion: v1
kind: Config
clusters:
  - name: cv-<cluster-id>
    cluster:
      server: https://<observed-workload-api-endpoint>
      certificate-authority-data: <observed-base64-ca>
users:
  - name: cv-<cluster-id>-oidc
    user:
      exec:
        apiVersion: client.authentication.k8s.io/v1
        command: kubectl
        args:
          - oidc-login
          - get-token
          - --oidc-issuer-url=https://login.example.com
          - --oidc-client-id=kubernetes-public
          - --grant-type=authcode
          - --oidc-pkce-method=S256
          - --oidc-extra-scope=groups
        interactiveMode: IfAvailable
        installHint: Install the approved kubectl oidc-login plugin.
contexts:
  - name: cv-<cluster-id>-<subtenant-id>-app
    context:
      cluster: cv-<cluster-id>
      user: cv-<cluster-id>-oidc
      namespace: st-<subtenant-id-without-hyphens>-app
current-context: cv-<cluster-id>-<subtenant-id>-app
```

Select scopes from the approved login profile; the example `groups` scope is not universal. The same kubeconfig does not manufacture group membership, override token claims or restrict an otherwise privileged identity. Context names use immutable platform IDs to avoid collisions across accounts and tenants.

The exec API version must match the plugin's ExecCredential output; `interactiveMode` is required for v1. The plugin executes on the user's machine, not the platform server. Use an allowlisted command and structured arguments; never accept arbitrary exec commands from a Sub Tenant request. [Kubernetes credential plugins](https://v1-35.docs.kubernetes.io/docs/reference/access-authn-authz/authentication/#client-go-credential-plugins)

Download uses observed endpoint/CA and effective OIDC configuration. Never copy the management controller's workload kubeconfig Secret into a response. Keep tokens/refresh tokens on the user's machine; do not persist them in management CRs, UI storage, API responses or audit logs. A private EKS endpoint still requires the user's VPN/network path.

## 5. RPC, CLI and dashboard plan

Propose a scoped `SubTenantService.GetSubTenantKubeconfig` request with tenant ID, cluster ID, Sub Tenant ID and optional namespace key/login mode. Return credential-free YAML, available contexts, and explicit OIDC/access conditions. No caller-supplied issuer, endpoint, CA, subject, scope expansion or plugin executable.

Authorize the exact allocation and namespace before resolving cluster metadata. Phase one can let authorized tenant administrators download/share the credential-free config. End-user self-service requires a scoped `subtenant.connect` permission or trusted issuer/group-to-allocation authorization, not blanket tenant-wide Cluster read rights. The platform session and workload IdP login may be different sessions/audiences; do not treat platform roles as workload group membership without a verified mapping.

Availability rules:

- Require owned, nondeleting allocation, observed endpoint/CA, and a matching active OIDC association. A desired `spec.access.oidc` alone is insufficient.
- Do not gate on aggregate Cluster Ready, Kubernetes upgrades or unrelated components. If the workload API is temporarily unreachable, expose the connection state without inventing another identity or authentication method.
- Config download is not an access grant. If baseline/access qualification or bindings are pending, return that state explicitly and explain that API requests remain forbidden; never label the namespace usable yet.
- OIDC disabled/unobserved: report the exact prerequisite. Do not silently fall back to IAM for a Sub Tenant end user.

CLI should reuse the shared builder and support allocation/namespace selection; final command syntax is an implementation decision. Write a new file with restrictive permissions and require explicit user intent before replacing/merging an existing kubeconfig. The existing cluster-level `--oidc` path remains supported.

Dashboard: retain one Get kubeconfig entry in the cluster header; when OIDC is configured offer Company sign-in, with IAM presented separately only to authorized IAM users. Add scoped Get kubeconfig to each Sub Tenant's action menu, with namespace selection. Bindings belong in the Sub Tenant create/edit form and YAML, with a review showing effective group names, role profiles and namespace grants/removals. No login credentials are requested or displayed by the dashboard.

## 6. Reconciliation, readiness and revocation

Preserve the architecture: the per-SubTenant management controller writes only its owned Capsule Tenant. Shared bootstrap/GTRs manage native RBAC. Change the singular `developerGroup` into a bounded `accessBindings` data structure translated exclusively from authorized management intent. Give bindings stable logical IDs based on principal/profile/scope, never list position. Protect this data and the access gate against direct tenant edits.

Observe separate conditions for `BaselineReady`, `OIDCReady`, `BindingsReady` and `AccessReady`, with generation, namespace inventory and effective profile/access revisions. Structural namespace provisioning can complete while access is blocked; report the actual outstanding condition rather than implying the controller is still creating already-existing resources. Do not make access Ready from GTR global Ready or namespace existence.

Before enabling access, require a trusted current baseline/qualification result and verify actual generated subjects, role references and namespace ownership. If qualification automation is not available, define a separately authorized, revision-bound operator qualification workflow; never ask the tenant to patch `access-ready=true`. The live NetworkPolicy comparison bug is a prerequisite repair, not a reason to bypass real drift.

For changed/removed groups, namespace scope or issuer settings: close the access gate, wait for obsolete generated bindings to disappear, write the new approved data, then requalify/observe the replacement bindings. Validate this behavior against pinned Capsule 0.15.2: a smaller generator output or selector change must actually remove old objects. Do not assume an Orphan policy revokes permissions. Normal changes respect freeze/release rules; any emergency access-revocation override must be a separate explicit authorization policy.

Removing an IdP group membership does not invalidate already-issued token claims immediately. Define token lifetimes and reauthentication policy; urgent revocation also needs the corresponding RBAC grant removed. Account for API authentication/authorization caches and existing watches/sessions. Kubernetes RBAC cannot express an explicit deny that overrides another grant.

Issuer/client/groups-prefix replacement must be planned: invalidate old binding resolution, prevent mixed old/new subject grants, observe the new association, and regenerate kubeconfigs. Keep an independently controlled operator recovery path, not a fallback credential in the tenant download.

## Delivery phases and acceptance

1. Repair semantic baseline comparison and clarify provisioning-versus-access status. Test omitted/empty policy lists without accepting allow-all drift.
2. Define group delegation policy, backwards-compatible access bindings, namespace scope, and shared OIDC login metadata/builder.
3. Implement trusted resolution and pinned GTR/RBAC changes with observed revocation and qualification gates.
4. Add scoped kubeconfig RPC, CLI reuse and dashboard download/form/review flows.
5. Run an isolated real-IdP end-to-end test before normal platform release rollout.

Acceptance matrix:

- IdP group A can read/create only its allowed `app` workloads; another group may view `worker`; unrelated group C authenticates but receives Forbidden.
- Use genuine OIDC sessions for these tests, not only operator `--as` impersonation. Verify token identity with `kubectl auth whoami`, then allowed and forbidden API calls and `kubectl auth can-i` checks.
- Changing a kubeconfig namespace cannot grant access to another Sub Tenant, `kube-system`, cluster-scoped resources, RBAC, Secrets, policy objects or service-account token issuance.
- Test overlapping group membership and unrelated preexisting grants, since effective permissions are the union.
- Wrong issuer/audience, absent groups, wrong prefixes, unknown/delegation-disallowed groups, directory group-overage behavior, unsupported login flow and missing plugin all produce actionable errors.
- Test refresh/relogin, browser/approved device login, private endpoint reachability, issuer rotation, group removal, partial GTR failure, restart and freeze.
- Download contains no static bearer token, client secret, private key, management credentials or arbitrary executable. Viewer/end-user scope cannot read another allocation's metadata through the download endpoint.
- A cluster upgrade or Karpenter failure does not block a ready Capsule/Sub Tenant access reconciliation. Actual Capsule/bootstrap/API failures remain visible.

## References

Verified 2026-11-16 against the sources linked above and existing [OIDC documentation](../oidc.md), [Sub Tenant isolation design](namespace-as-a-service.md), and [test guide](../testing/subtenancy.md). This document proposes future behavior; examples are not instructions to apply resources now.
