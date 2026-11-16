# Qualify SubTenant OIDC namespace access

Provisioning and access are separate. `ProvisioningReady=True` and `Ready=True`
mean Capsule has acknowledged the current Tenant and all requested namespace
baselines are observed. They do not grant human access. `AccessReady=True`
additionally requires the current ACTIVE EKS OIDC association, a trusted runtime
qualification, propagated namespace readiness and exact generated RoleBindings.
With groups configured before OIDC exists, provisioning can complete while
`AccessReady=False`, reason `OIDCPending`.

There is **no automatic runtime qualifier**. This procedure is an explicit
platform-operator attestation after real tests, not proof manufactured from
static YAML, a Capsule Ready condition, or a UI checkbox. No tenant-facing API
can create an attestation. The commands below are operator instructions; they
are not executed by the controller or this document.

## Trust and prerequisites

The workload cluster's `naas-system` namespace is the qualification authority.
It must be a platform namespace, not owned/labeled as a Capsule allocation.
Only the platform administrator or a narrowly authorized qualification operator
may create/update/delete its qualification ConfigMaps. Review **all additive
RBAC grants**, including unrelated ClusterRoleBindings. Neither SubTenant
administrator nor viewer groups may write ConfigMaps there. Their fixed roles
are granted only by RoleBindings in their verified allocated namespaces; the
GTR refuses foreign Namespace owner UIDs and unallocated names.

Test with actual tenant credentials that `kubectl auth can-i create configmaps
-n naas-system` and its `update`, `patch`, `delete` equivalents all answer `no`.
Do not distribute the operator kubeconfig, grant cluster-admin to a tenant,
create human Capsule owners, or patch access-ready true by hand.

Before qualification, install the current versioned Capsule bootstrap and
required protected admission policies. Configure the parent's **human** EKS OIDC
provider, including its groups claim/prefix; IRSA is not human OIDC. The
controller verifies one nondeleting, Ready/Synced, ACTIVE provider association
whose desired and observed mapping, physical cluster name/account/region, and
immutable association UID match the parent. Username defaults follow the
[documented EKS rules](https://docs.aws.amazon.com/eks/latest/APIReference/API_OidcIdentityProviderConfigRequest.html);
issuer, audience, groups and required claims remain exact.

## Emit a current test binding without hand-copying hashes

From the repository root, use explicit approved contexts and private local
kubeconfig paths. This command reads management intent/provider observations
and workload Tenant/namespace/bootstrap/RBAC objects. It writes only JSON to
stdout, does not read Kubernetes Secrets, applies nothing, and does not set any
test result to passing:

```sh
go run ./pkg/naas/cmd/access-qualification \
  --management-context management-operator \
  --management-kubeconfig /private/path/management-kubeconfig \
  --workload-context workload-qualification-operator \
  --workload-kubeconfig /private/path/workload-kubeconfig \
  --namespace tenant-acme \
  --allocation REPLACE_WITH_SUBTENANT_UUID > request.before.json

jq '{evidence:"", checks:(.requiredChecks | map({key:., value:false}) | from_entries)}' \
  request.before.json > test-results.json
```

The request includes the protected ConfigMap name/namespace and exact binding:
allocation/registration/parent/remote Tenant UIDs, parent cluster ID, namespace
UIDs, canonical rendered security-spec hash, bootstrap revision, resolved OIDC
revision and observed association UID. These values come from fresh reads,
not supplied SubTenant status. Cosmetic display-name and unrelated cluster
scaling/maintenance edits do not invalidate qualification. Changes to groups,
quota, requested namespaces, security templates, OIDC mapping or any bound UID
do. Expiry still applies even when configuration is unchanged.

If the request cannot be emitted, inspect the SubTenant's current conditions and
restore parent/OIDC/bootstrap/namespace readiness. Do not fill missing bindings
from guesses or use old status to bypass the prerequisite check.

## Run and record the actual tests

Use [the SubTenancy test walkthrough](subtenancy.md), with the actual current
admin/viewer group mappings. Required results are:

| Check key | Evidence required |
| --- | --- |
| `namespace-rbac` | Actual scoped administrator/viewer identities have exactly their intended workload permissions. Viewer cannot write, exec, request tokens, or read Secrets. |
| `cross-namespace-rbac` | Both roles are denied foreign namespaces, RBAC/security-policy writes, namespace management and platform resources. Both are denied writes to `naas-system` ConfigMaps. |
| `admission-guards` | Protected readiness/enrollment/PSS metadata and privileged/service-account workload escapes are enforced, including controller-generated Pods. |
| `ambient-mtls-hbone` | Same-allocation healthy traffic works over authenticated ambient/HBONE; plaintext escape probes are denied. |
| `cross-subtenant-traffic` | Healthy A/B controls work, but traffic between allocations fails for Service and pod IPs, including across nodes. |
| `dns-udp-tcp` | Both DNS transports work with the actual native CNI/resolver setup. |
| `egress-default-deny` | Controlled healthy external/nonmesh sinks and metadata/privileged node endpoints remain denied unless explicitly approved. Never request cloud credentials as a probe. |
| `restricted-pss` | Noncompliant Pods are rejected for the expected policy reason; a Deployment dry-run alone is not Pod enforcement evidence. |
| `shared-quota` | A bounded cross-namespace test demonstrates the allocation-wide budget and correct accounting. |
| `group-delegation-approved` | The platform operator verifies that these exact issuer-bound group claims may be delegated by this tenant; typing a group name is not directory ownership proof. |

There is no circular requirement to open production access before testing:
the qualification operator explicitly installs temporary, namespace-scoped test
bindings for the exact approved test groups/identities. Use the fixed workload
roles; do not add human Capsule owners. The reviewed admission policy must have
a narrow qualification-runner path for disposable test workloads while normal
tenant access is closed. If that path or another prerequisite is absent, mark
the affected test blocked and do not attest it as passed. Impersonation alone
does not prove real OIDC login; include actual approved OIDC user credentials in
authentication tests without exposing their tokens.

After testing, remove temporary bindings and disposable workloads through the
reviewed lab cleanup procedure. **Remove all temporary test RoleBindings before
the final request**: the request builder and controller reject unexpected RBAC.
Do not remove generated baseline policies, service accounts, namespace ownership
or finalizers. Enter `true` in `test-results.json` only for tests actually passed,
and set `evidence` to a nonsecret retained test-report reference. Do not paste
tokens, kubeconfigs, Secret data or directory credentials into that record.

## Publish a short-lived qualification as the platform operator

Rerun the same read-only request command above into `request.after.json` after
cleanup. Compare the security bindings:

```sh
jq -S .binding request.before.json > binding.before.json
jq -S .binding request.after.json > binding.after.json
cmp binding.before.json binding.after.json
```

If they differ, rerun the affected runtime suite against the new configuration;
do not copy old checks onto an untested binding. If unchanged and every test
passed, assemble a 30-minute record from the real results:

```sh
python3 - request.after.json test-results.json > qualification.json <<'PY'
import datetime, json, sys
request, results = (json.load(open(path)) for path in sys.argv[1:])
assert set(results['checks']) == set(request['requiredChecks'])
assert all(value is True for value in results['checks'].values())
assert isinstance(results['evidence'], str) and results['evidence'].strip()
now = datetime.datetime.now(datetime.timezone.utc)
record = dict(version='v1', binding=request['binding'],
              issuedAt=now.isoformat(),
              expiresAt=(now + datetime.timedelta(minutes=30)).isoformat(),
              evidence=results['evidence'], checks=results['checks'])
json.dump(record, sys.stdout, indent=2)
PY

# Review the exact protected destination and evidence before this explicit write.
QUALIFICATION_NAME=$(jq -r .name request.after.json)
QUALIFICATION_NAMESPACE=$(jq -r .namespace request.after.json)
test "$QUALIFICATION_NAMESPACE" = naas-system
kubectl --context workload-qualification-operator \
  --kubeconfig /private/path/workload-kubeconfig -n "$QUALIFICATION_NAMESPACE" \
  create configmap "$QUALIFICATION_NAME" \
  --from-file=qualification.json --dry-run=client -o yaml |
  kubectl --context workload-qualification-operator \
    --kubeconfig /private/path/workload-kubeconfig apply -f -
```

The controller rejects missing/oversized/malformed records, unknown fields,
missing/failed checks, mismatched bindings, expired records and validity periods
longer than **one hour**. It rechecks live EKS OIDC and current namespace/security
state rather than accepting the record's claims about provider readiness.
Once qualified, it opens only the owned Tenant's access gate; Capsule generates
the namespace RoleBindings. `AccessReady` remains false with
`AccessBindingsPending` until namespace readiness and exact Group/ClusterRole
bindings are actually observed. No User object, human owner or token is created.

## Renewal and revocation

Renew before expiry by repeating the current-binding/runtime-evidence/cleanup
procedure and replacing the same protected ConfigMap. An unchanged current
binding and valid renewal do not close/reopen the gate. There is no automatic
test runner or automatic extension of old evidence. Security changes require
fresh qualification; unrelated edits do not.

To revoke explicitly, the platform operator deletes only this attestation
ConfigMap (using its reviewed name/namespace). Missing/expired/changed evidence,
OIDC mismatch, namespace UID changes or unexpected RBAC make the controller
close its gate. Capsule's `deletion: Remove` policy subsequently removes the
generated group bindings. Management group intent is retained for review.

**This is not an authentication token TTL or instantaneous RBAC revocation.**
Expiry is evaluated during reconciliation. A frozen, stopped, unreachable or
unauthorized controller cannot guarantee revocation at the expiry instant, and
Capsule removal is asynchronous. `AccessReady=False` means the controller does
not consider access ready; it does not prove every old binding/session has
already disappeared. Inspect actual RoleBindings and restore controller/Capsule
health; use the platform incident-response procedure for urgent revocation.
