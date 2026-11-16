# Test SubTenancy from a workload-cluster tenant identity

This is a staged lab procedure, not a claim that a provisioned namespace is
already isolated. It covers **two SubTenants on the same target workload
cluster**, with tenant A owning two namespaces and tenant B owning another.
Commands are instructions for an approved test run; authoring this guide does
not apply resources, read Kubernetes Secrets, or delete anything.

## What can be tested now

The management [CRD](../../api/v1alpha1/subtenant_types.go),
[CRUD RPCs](../../idl/cloud/v1/subtenant_service.proto), cluster-detail Sub Tenant
UI, and parent deletion/NaaS-disable safety guards exist. Management intent is
persisted in `tenant-<tenantID>`. Creation acceptance is not remote readiness.
The per-allocation workload controller is wired into the manager: check
the deployed build and effective Capsule release before expecting remote resources. Its
[Tenant renderer](../../pkg/naas/tenant.go) derives a stable `st-<UUID without
hyphens>` name, validates immutable ownership, pins PSS to the observed
Kubernetes minor and always renders access-ready false. Its
[workload client provider](../../pkg/management/subtenant_workloads.go) is an
internal controller credential path, not a tenant kubeconfig distribution API.
The
[bootstrap library](../../pkg/naas/bootstrap.go) loads versioned templates
offline; its presence alone does not install those templates.

`Pending` with `CapsulePending` or `BootstrapPending` means the
controller is waiting and will retry automatically. `PrerequisitesReady=False`
and `status.message` identify the dependency. Capsule failures preserve
their blocker message; aggregate cluster readiness and Kubernetes upgrades do
not gate provisioning while Capsule and its bootstrap are ready and the workload
API is reachable. Developer access reports `PrerequisitesPending`
until provisioning prerequisites are observed. Once they are ready, access can
still report `QualificationRequired`; that is a separate outstanding gate.

There is currently **no configured identity resolver or automatic traffic
qualification**. Nonempty `spec.access.bindings` is refused with
`FAILED_PRECONDITION`. Keep the remote Capsule Tenant's protected
`cloud.cluster-vending.io/access-ready` label **false**. Do not patch it true,
forge `Ready`/qualification status, add human Capsule owners, or grant admin to
get a test to pass. A future controller may report baseline configuration
complete while access remains closed; that is different from qualified traffic
and from management `Ready=True` for the current generation/revision.

The intended chain is:

```text
management SubTenant intent (canonical tenant/cluster refs + UIDs)
  -> workload Capsule Tenant (per-allocation writer owns this kind only)
  -> shared Capsule GlobalTenantResources (GTRs)
     -> generated Namespaces, NetworkPolicies, Istio policies, LimitRange,
        app/default ServiceAccounts; Capsule quota rules -> shared quota
     -> developer RoleBindings only after trusted qualification/access gate
```

Missing remote controller, bootstrap, execution RBAC, identity resolution,
protected admission, or qualification is a **blocked stage**, not a passed test.
Do not manually create the remote Tenant or children to disguise a failed
management-to-workload reconciliation test. An independently approved bootstrap
lab can test the templates, but its result must be recorded separately.

## 1. Identify the release, cluster, and actors

Use a disposable workload cluster and approved test data only. Record the
management build/release, actual cluster platform release, Capsule chart/app
version, Kubernetes server/minor, Istio/ztunnel/CNI images and configuration,
native CNI enforcement mode, PSS version, shared template revision, and namespace
UIDs. The current catalog pins Capsule **0.15.2** and Istio/ztunnel **1.32.0**;
an existing cluster can still run an older release. Check the actual installation
against [the catalog](../../pkg/catalog/release.yaml) and
[Capsule integration](../components/capsule.md), not just those defaults.

Separate three actors:

* **Management operator:** creates/reviews allocation intent and inspects its CR.
* **Workload test operator:** inspects platform policies, installs reviewed lab
  fixtures, and correlates telemetry. Its credentials are never given to tenants.
* **Tenant A / tenant B:** distinct authenticated, namespace-scoped test users.
  Every tenant assertion below uses these users, not the operator context.

Set explicit context names; the paths below identify private local kubeconfigs,
not files to commit or share. Never print kubeconfig contents, bearer tokens,
Secret data, or `kubectl config view --raw` output into the test record.

```sh
MGMT_CONTEXT=management-test
WORKLOAD_OPERATOR_CONTEXT=workload-test-operator
TENANT_ID=acme
MGMT_NS="tenant-$TENANT_ID"
CLUSTER_NAME=replace-with-management-cluster-name
CLUSTER_ID=replace-with-cluster-uuid
A_ID=replace-with-subtenant-a-uuid
B_ID=replace-with-subtenant-b-uuid
CLUSTER_DOMAIN=cluster.local # operator verifies the actual cluster DNS suffix

km() { kubectl --context "$MGMT_CONTEXT" "$@"; }
ko() { kubectl --context "$WORKLOAD_OPERATOR_CONTEXT" "$@"; }

km -n "$MGMT_NS" get clusters.cloud.cluster-vending.io "$CLUSTER_NAME" -o json |
  jq '{name:.metadata.name,uid:.metadata.uid,naas:.spec.naas,
       platformRelease:.status.platformRelease,components:.status.platformComponents}'
ko version -o yaml
ko -n capsule-system get deployments -o wide
ko -n istio-system get deployments,daemonsets -o wide
ko -n kube-system get daemonsets -o wide
ko api-resources --api-group=capsule.clastix.io
```

Capsule requires the workload cluster's own working cert-manager; a management
cluster cert-manager is insufficient. Also require working ambient enrollment,
native NetworkPolicy enforcement, the three GTRs, constrained replication
identity/RBAC, and reviewed admission guards protecting namespace membership,
PSS/ambient/readiness metadata, service accounts and policy objects. These guards
are prerequisites, not functionality supplied by the GTR YAML itself. Verify the
live installation; do not assume a shipped design template installed them.

## 2. Create and trace management intent

In the selected tenant's cluster detail page, enable NaaS through the reviewed
cluster update flow, then open **Sub Tenants → Create Sub Tenant → Paste YAML**.
Use [examples/subtenant.yaml](../../examples/subtenant.yaml), with the real
`metadata.tenant` and `metadata.clusterId`. Create A with namespace keys
`[app, worker]`, namespace count at least two, and B with `[app]`. Leave access
bindings empty. Give each enough budget for two small probe replicas per
namespace; quota-specific tests below need their own deliberately small budget.

That YAML uses `cluster-vending.io/v1`, an API/UI envelope. **Do not kubectl apply
it.** The service assigns the ID, retry identity, labels and immutable native
tenant/cluster references. The
[native sample](../../config/samples/cloud_v1alpha1_subtenant.yaml) instead uses
`cloud.cluster-vending.io/v1alpha1` and illustrates the stored record. Do not
copy its illustrative UIDs or bypass the service's ownership checks. There is
no SubTenant CLI subcommand at present.

`cloud.v1.SubTenantService` exposes `CreateSubTenant`, `GetSubTenant`,
`ListSubTenants`, `UpdateSubTenant` and `DeleteSubTenant`.
All require tenant scope; Get/Update/Delete also
require the bound cluster ID. Updates/deletes need the latest etag. Only
`spec.display_name`, `spec.namespace_keys`, `spec.quota`, `spec.policy_profile`
and `spec.access` are mutable through field masks. Namespace keys are append-only.
Viewer sessions can inspect; mutations require tenant administration. Neither
role grants Kubernetes access to the target workload cluster.

Inspect the resulting native records as the management operator:

```sh
km -n "$MGMT_NS" get subtenants.cloud.cluster-vending.io "$A_ID" -o yaml
km -n "$MGMT_NS" get subtenants.cloud.cluster-vending.io "$B_ID" -o yaml
```

Expected: `spec.tenantRef` identifies the current registration UID;
`spec.clusterRef` has this cluster's ID/name/UID; labels agree. Watch the actual
controller's conditions, observed generation, `remoteCapsuleTenant`, remote UID,
`namespaces[]` and template revision. Never treat an empty status as success.
Once remote status exists, obtain names from status, not display names or guessed
namespace prefixes:

```sh
A_REMOTE=$(km -n "$MGMT_NS" get subtenants.cloud.cluster-vending.io "$A_ID" \
  -o jsonpath='{.status.remoteCapsuleTenant}')
A_APP=$(km -n "$MGMT_NS" get subtenants.cloud.cluster-vending.io "$A_ID" \
  -o jsonpath='{.status.namespaces[?(@.key=="app")].name}')
A_WORKER=$(km -n "$MGMT_NS" get subtenants.cloud.cluster-vending.io "$A_ID" \
  -o jsonpath='{.status.namespaces[?(@.key=="worker")].name}')
B_REMOTE=$(km -n "$MGMT_NS" get subtenants.cloud.cluster-vending.io "$B_ID" \
  -o jsonpath='{.status.remoteCapsuleTenant}')
B_APP=$(km -n "$MGMT_NS" get subtenants.cloud.cluster-vending.io "$B_ID" \
  -o jsonpath='{.status.namespaces[?(@.key=="app")].name}')
test -n "$A_REMOTE" && test -n "$A_APP" && test -n "$A_WORKER" &&
  test -n "$B_REMOTE" && test -n "$B_APP" # stop if any name is absent
```

## 3. Inspect configured baseline as the operator

```sh
ko get tenants.capsule.clastix.io "$A_REMOTE" "$B_REMOTE" -o yaml
ko get globaltenantresources.capsule.clastix.io \
  naas-namespace-bootstrap naas-isolation-baseline naas-developer-access -o yaml
ko get namespaces "$A_APP" "$A_WORKER" "$B_APP" -o yaml
ko -n "$A_APP" get networkpolicies,peerauthentications.security.istio.io,authorizationpolicies.security.istio.io,limitranges -o yaml
ko -n "$A_APP" get serviceaccounts app default -o yaml
ko -n "$A_APP" get rolebindings -o yaml
ko get globalresourcequotas.capsule.clastix.io -o yaml
```

Compare all three namespaces with the
[canonical bootstrap](../../pkg/naas/assets/bootstrap.yaml):

| Object/check | Expected configuration; still needs runtime qualification |
| --- | --- |
| Namespace | Correct Capsule Tenant owner UID and membership; distinct allocation IDs; ambient enrollment; pinned restricted PSS; `naas-ready=false` while access is closed. |
| `naas-deny-by-default` | Ingress and egress default deny. |
| `naas-allow-subtenant-hbone` | TCP 15008 ingress/egress scoped to the same Capsule Tenant. |
| `naas-allow-cluster-dns` | UDP/TCP 53 to selected kube-system CoreDNS pods. NodeLocal DNS or different labels require separate qualification. |
| `naas-require-mtls` | Istio `PeerAuthentication` STRICT. |
| `naas-allow-owned-namespaces` | Istio ALLOW source namespaces exactly this allocation's generated names. |
| `naas-container-defaults`, `app`/`default` SAs | Reviewed limits/defaults; token automount false. |
| Capsule quota | One allocation-wide budget from Tenant rules, not a fresh copy for every namespace. Confirm generated quota selectors, hard values and accounting. |
| Developer access | No generated `naas-developers` binding while access-ready is false. |

GTR Ready and correct YAML prove replication/configuration only. Namespace,
network and Istio enforcement can still be incomplete or incompatible. The
[Istio NetworkPolicy guide](https://istio.io/latest/docs/ambient/usage/networkpolicy/)
explains why ambient HBONE and native policy must be evaluated together.

## 4. Use a tenant identity, without opening the production access gate

The operator must approve **temporary lab identities/RBAC** in these disposable
namespaces. [subtenant-test-access.yaml](../../examples/subtenant-test-access.yaml)
binds one explicit User to the existing narrow `naas-workload-editor` role and
adds namespace-local `pods/exec` for traffic probes. Review the live ClusterRole
first; it must not have acquired wildcard, Secret, RBAC or policy privileges.
Bind user A only in A_APP/A_WORKER, and user B only in B_APP. These are test
fixtures, not the missing identity resolver or an automatically qualified grant.
For real scoped credentials, bind the exact authenticated username reported by
`auth whoami`; `naas-test-a`/`naas-test-b` below are example approved impersonation
subjects, not an identity mapping created by this fixture.

The RoleBinding does **not** bypass an admission readiness gate. If that gate
blocks test workload creation, retain the rejection as a result. Runtime tests
then require an operator-approved qualification harness/identity permitted by the
reviewed admission policy, narrowly confined to these lab namespaces. Without
that facility, mark runtime tests **blocked**; do not weaken production guards,
flip access-ready, enroll a human as Capsule administrator, or claim end-user
onboarding passed. Ensure Capsule admission applies to the chosen test identity;
an ignored/system identity cannot prove user-facing quota/admission enforcement.

For a reviewed lab run, the operator renders and applies the RBAC fixture:

```sh
# Run from the repository root. envsubst is needed only for these test fixtures.
SUBTENANT_TEST_USER=naas-test-a
export SUBTENANT_TEST_USER
envsubst '${SUBTENANT_TEST_USER}' < examples/subtenant-test-access.yaml |
  ko -n "$A_APP" apply -f -
envsubst '${SUBTENANT_TEST_USER}' < examples/subtenant-test-access.yaml |
  ko -n "$A_WORKER" apply -f -
SUBTENANT_TEST_USER=naas-test-b
envsubst '${SUBTENANT_TEST_USER}' < examples/subtenant-test-access.yaml |
  ko -n "$B_APP" apply -f -
```

Prefer separately issued, short-lived scoped user kubeconfigs:

```sh
A_KUBECONFIG=/private/path/to/tenant-a-kubeconfig
B_KUBECONFIG=/private/path/to/tenant-b-kubeconfig
A_CONTEXT=tenant-a-test
B_CONTEXT=tenant-b-test
ka() { kubectl --kubeconfig "$A_KUBECONFIG" --context "$A_CONTEXT" "$@"; }
kb() { kubectl --kubeconfig "$B_KUBECONFIG" --context "$B_CONTEXT" "$@"; }
ka auth whoami
kb auth whoami
```

Alternatively, only the test operator may use impersonation:

```sh
# Use instead of the ka/kb definitions above, never as a tenant credential.
ka() { kubectl --context "$WORKLOAD_OPERATOR_CONTEXT" --as=naas-test-a "$@"; }
kb() { kubectl --context "$WORKLOAD_OPERATOR_CONTEXT" --as=naas-test-b "$@"; }
ka auth whoami
kb auth whoami
```

The operator must already have explicit, narrowly approved `impersonate` rights
for those exact users (and any explicitly used groups). Do not grant impersonation
to tenants, use `system:masters`, or distribute the operator kubeconfig. Supply
only the approved group claims if the tested identity actually requires them.
See [Kubernetes impersonation](https://kubernetes.io/docs/reference/access-authn-authz/user-impersonation/).
Impersonation tests RBAC/admission as that subject; it does not prove the real
OIDC/IAM login path or future identityRef resolution. Repeat with actual scoped
credentials before qualifying end-user authentication.

## 5. Check tenant visibility and denied capabilities

Run each query with ka, then repeat with kb and swapped namespaces. `no` is the
expected authorization answer for negative cases; connection/authentication
errors or discovery failures are **not** successful denials. Do not fetch Secret
contents to test lack of access.

```sh
ka auth can-i list pods -n "$A_APP"                # yes
ka auth can-i create deployments.apps -n "$A_APP"  # yes (RBAC only)
ka auth can-i create configmaps -n "$A_WORKER"     # yes
ka auth can-i list pods -n "$B_APP"                # no
ka auth can-i list pods --all-namespaces            # no
ka auth can-i list namespaces                      # no
ka auth can-i create namespaces                    # no
ka auth can-i patch namespaces                     # no
ka auth can-i get nodes                            # no
ka auth can-i get secrets -n "$A_APP"              # no
ka auth can-i list secrets -n "$B_APP"             # no
ka auth can-i create rolebindings.rbac.authorization.k8s.io -n "$A_APP" # no
ka auth can-i bind clusterroles.rbac.authorization.k8s.io              # no
ka auth can-i escalate roles.rbac.authorization.k8s.io -n "$A_APP"     # no
ka auth can-i impersonate users                    # no
ka auth can-i patch networkpolicies.networking.k8s.io -n "$A_APP"      # no
ka auth can-i patch authorizationpolicies.security.istio.io -n "$A_APP" # no
ka auth can-i patch peerauthentications.security.istio.io -n "$A_APP"  # no
ka auth can-i patch tenants.capsule.clastix.io        # no
ka auth can-i create globaltenantresources.capsule.clastix.io # no
ka auth can-i create pods -n "$A_APP"              # no: use workload controllers
ka auth can-i create pods --subresource=exec -n "$A_APP" # yes: test-only fixture
ka -n "$A_APP" get pods                            # allowed; possibly empty
ka -n "$B_APP" get pods                            # Forbidden; never B's data
```

Native Kubernetes does not return a tenant-filtered Namespace list merely
because namespaced RoleBindings exist. Use the allocated namespace names from
the management UI/status. Capsule proxy is not enabled by this integration.
Do not grant cluster-wide listing as a workaround.

## 6. Create harmless probes as the tenant, then test traffic

This stage needs the approved test admission path described above. Use an
operator-approved Python 3 image pinned by digest, available to the nodes without
new image-pull credentials. The fixture has no volumes/tokens, runs non-root,
drops capabilities, uses restricted-compatible settings, and exposes only a
fixed HTTP response. The image must support `python` with the standard library.

```sh
SUBTENANT_TEST_IMAGE='replace-with-approved-python-image@sha256:digest'
export SUBTENANT_TEST_IMAGE
PROBE_RENDERED=$(mktemp)
envsubst '${SUBTENANT_TEST_IMAGE}' < examples/subtenant-test-workload.yaml > "$PROBE_RENDERED"
# Review this rendered, nonsecret file before applying it.
ka -n "$A_APP" apply -f "$PROBE_RENDERED"
ka -n "$A_WORKER" apply -f "$PROBE_RENDERED"
kb -n "$B_APP" apply -f "$PROBE_RENDERED"
ka -n "$A_APP" rollout status deployment/subtenant-probe --timeout=180s
ka -n "$A_WORKER" rollout status deployment/subtenant-probe --timeout=180s
kb -n "$B_APP" rollout status deployment/subtenant-probe --timeout=180s
ka -n "$A_APP" get pods -o wide
ka -n "$A_WORKER" get pods -o wide
kb -n "$B_APP" get pods -o wide
```

A rollout timeout is not proof of isolation: first distinguish readiness-gate,
admission, quota, image, scheduling and node-capacity failures using namespaced
Events. Verify both A and B are healthy from their own identities before using
cross-allocation failures as evidence. Operator correlates pod IPs/nodes; repeat
on the same node and different nodes/AZs where supported. A self-call alone does
not qualify inter-pod isolation.

```sh
http_a() {
  ka -n "$A_APP" exec deployment/subtenant-probe -- python -c \
    'import sys,urllib.request; o=urllib.request.build_opener(urllib.request.ProxyHandler({})); r=o.open(sys.argv[1],timeout=5); print(r.status,r.read(64).decode())' "$1"
}
http_b() {
  kb -n "$B_APP" exec deployment/subtenant-probe -- python -c \
    'import sys,urllib.request; o=urllib.request.build_opener(urllib.request.ProxyHandler({})); r=o.open(sys.argv[1],timeout=5); print(r.status,r.read(64).decode())' "$1"
}
http_a "http://subtenant-probe.$A_APP.svc.$CLUSTER_DOMAIN:8080/"    # 200
http_a "http://subtenant-probe.$A_WORKER.svc.$CLUSTER_DOMAIN:8080/" # 200
http_b "http://subtenant-probe.$B_APP.svc.$CLUSTER_DOMAIN:8080/"    # 200 control
http_a "http://subtenant-probe.$B_APP.svc.$CLUSTER_DOMAIN:8080/"    # denied
http_b "http://subtenant-probe.$A_APP.svc.$CLUSTER_DOMAIN:8080/"    # denied
```

Require positive controls to return `200 subtenant-probe`. Negative traffic must
never return the remote application's response; a reset, mesh rejection or
timeout needs correlated policy/flow evidence, not just an arbitrary curl exit.
DNS resolution of a foreign Service may still succeed: DNS names are not a
confidentiality boundary. Repeat against operator-supplied Service IPs and actual
other-pod IPs to separate DNS, service routing and direct-pod behavior.

| Probe, from actual tenant workloads | Required result/evidence |
| --- | --- |
| Same A namespace, different pod; A app → A worker and reverse | HTTP succeeds. Trace identifies ambient/mTLS path, source identity and destination; repeat across nodes. |
| A → B and B → A, Service IP and pod IP | No application access. Native CNI drops and/or Istio authorization denies are correlated to these exact endpoints. |
| DNS UDP **and TCP** | Both work to the configured resolver; do not add broad kube-system/node egress just to make DNS pass. |
| HBONE TCP 15008 | Same-allocation application traffic uses HBONE successfully; cross-allocation traffic cannot carry an accepted request. A TCP connect alone does not prove HBONE authentication or authorization. |
| Unenrolled/plaintext lab source → A | Denied by the intended native/STRICT boundary. Operator prepares the isolated source; tenants must not be allowed to disable ambient enrollment. |
| A → operator-owned nonmesh HTTP sink, approved external HTTPS sink/IP, node/kubelet endpoints and metadata endpoint | Denied except explicitly documented platform exceptions. Use harmless connectivity probes only; never request instance credentials or metadata credential paths. |
| Node readiness/liveness probes | Legitimate probes work with the qualified CNI/ambient node-probe handling; no blanket node/CIDR allow rule. |

For egress tests, have the operator provide **healthy controlled sink URLs** and
repeat `http_a "$APPROVED_SINK_URL"`; an unavailable Internet site is not a
negative control. The bootstrap does not define a general egress gateway or a
universal CNI-specific node/metadata exception. Unexpected reachability or broken
positive traffic means **not qualified**. Never remove default-deny/STRICT to
turn that into a pass. Keep access-ready false.

Run an explicit DNS transport check inside a tenant probe (Python stdlib only):

```sh
ka -n "$A_APP" exec -i deployment/subtenant-probe -- python - "$CLUSTER_DOMAIN" <<'PY'
import socket, struct, sys
resolver = next(line.split()[1] for line in open('/etc/resolv.conf')
                if line.startswith('nameserver '))
name = 'kubernetes.default.svc.' + sys.argv[1]
q = struct.pack('!HHHHHH', 1234, 0x0100, 1, 0, 0, 0)
q += b''.join(bytes([len(label)]) + label.encode() for label in name.split('.'))
q += b'\0' + struct.pack('!HH', 1, 1)
family = socket.AF_INET6 if ':' in resolver else socket.AF_INET
for transport in (socket.SOCK_DGRAM, socket.SOCK_STREAM):
    with socket.socket(family, transport) as s:
        s.settimeout(5)
        s.connect((resolver, 53))
        if transport == socket.SOCK_DGRAM:
            s.send(q)
            reply = s.recv(4096)
        else:
            s.sendall(struct.pack('!H', len(q)) + q)
            stream = s.makefile('rb')
            size = struct.unpack('!H', stream.read(2))[0]
            reply = stream.read(size)
        ident, flags, _, answers, _, _ = struct.unpack('!HHHHHH', reply[:12])
        assert ident == 1234 and flags & 15 == 0 and answers > 0
        print('UDP' if transport == socket.SOCK_DGRAM else 'TCP', 'DNS OK')
PY
```

The operator records ztunnel workload/connection and native-CNI flow metadata
for the tested endpoints, without dumping keys, certificates or Kubernetes
Secrets. Istio authentication identity and policy decisions, plus working
positive/negative controls, are stronger evidence than namespace labels alone.

## 7. Test admission and shared quota as the tenant

`auth can-i` proves authorization, not admission. In the approved lab test path,
use server dry-run for a harmless non-ClusterIP Service request:

```sh
ka -n "$A_APP" create service nodeport subtenant-negative \
  --tcp=8080:8080 --dry-run=server -o name
# Expected: denied by the service/admission boundary; no Service persists.
```

For PSS, create a disposable Deployment based on the probe with a single
prohibited field (`privileged: true`, hostNetwork, hostPath or root execution).
The tenant role can create Deployments, **not Pods**. A Deployment server dry-run
does not exercise admission of its future Pods. During an approved persistent
lab test, the Deployment may be accepted but its Pods must be rejected; inspect
`ka -n "$A_APP" get events --field-selector reason=FailedCreate` and Pod absence.
Require the exact PSS/guard denial; RBAC, readiness-gate, scheduling, quota or
image errors do not prove PSS. Never run a privileged pod to test the boundary.
Repeat service-account substitution, ambient-disable annotation, namespace
membership/readiness/PSS label tampering, NodePort/LoadBalancer/externalIPs and
protected policy/RBAC writes. Direct security-object/namespace edits should be
Forbidden; workload-template escapes need the admission guard, including Pods
created by ReplicaSets/Jobs. Stop qualification on any successful escape.

For a bounded aggregate-quota test, the operator measures the allocation's
existing ConfigMap usage (including generated system ConfigMaps) and sets
`quota.hard.configmaps` to **baseline usage + 2** through the reviewed SubTenant
update. Ensure there is no independent lower namespace quota masking this test.
With fresh unused names, tenant A then runs:

```sh
ka -n "$A_APP" create configmap subtenant-quota-one --from-literal=probe=nonsecret
ka -n "$A_WORKER" create configmap subtenant-quota-two --from-literal=probe=nonsecret
ka -n "$A_APP" create configmap subtenant-quota-three --from-literal=probe=nonsecret
```

Expected: the first two succeed; the third is denied for the **shared A budget**.
Moving the third to A_WORKER must not create another allowance. The operator
checks generated GlobalResourceQuota/ledger accounting; tenant B's independently
budgeted control still succeeds. Stop after the first expected denial; do not
fill a cluster with an unbounded loop. A dry-run-only test cannot establish
persisted aggregate usage. Also check actual probe requests/limits and approved
missing-resource defaulting against the generated LimitRange.

Direct Secret/RBAC denial is not a promise that a workload creator cannot use a
Secret or service account already present **in its own namespace**. Keep real
secrets out of this lab and enforce the intended Pod/service-account admission
boundary. Namespaces share worker kernels and cluster control-plane components;
this is not VM-style hard isolation for hostile tenants.

## 8. Verify template updates and freeze/deletion safety

The platform operator rolls out a reviewed change to the **shared** bootstrap
through the approved release/bootstrap path, not ad hoc per-namespace edits.
A useful non-weakening lab change is an accommodated LimitRange default request
increase. Keep the supported `restricted-ambient-v1` profile; inventing `v2` is
not a supported API update. Record the new canonical template revision.

The GTRs request a 60s resync; observe for a bounded period (for example five
minutes) and inspect replication errors rather than assuming a fixed sleep
proves convergence:

```sh
ko get globaltenantresources.capsule.clastix.io naas-isolation-baseline -o yaml
ko -n "$A_APP" get limitrange naas-container-defaults -o yaml
ko -n "$A_WORKER" get limitrange naas-container-defaults -o yaml
ko -n "$B_APP" get limitrange naas-container-defaults -o yaml
km -n "$MGMT_NS" get subtenants.cloud.cluster-vending.io "$A_ID" "$B_ID" -o yaml
```

Expected: **existing** namespaces receive the revised generated baseline; Tenant
and namespace owner UIDs stay stable; reported template revision matches the
deployed templates once observed. Re-run tenant RBAC, traffic and admission tests
for that revision/CNI/mesh combination. GTR status or a changed hash alone is
not qualification, and does not authorize setting access-ready true.

In a disposable lab, separately review these management actions in the UI/API:

| Scenario | Expected result |
| --- | --- |
| Freeze the cluster, then request an allocation update | Management controller must synchronize provider pause before safety checks and hold remote writes. No claim of instant packet/RBAC revocation. |
| Native cluster `spec.naas: false` while a child exists, including during freeze | `SubTenantSafety=False` with `SubTenantsRemain`, plus frozen status while held; native and canonical contract observed generations do not acknowledge the blocked edit. Unfreezing still blocks. |
| RPC NaaS disable (including validate-only) or cluster delete, including force, while children exist/terminate | `FAILED_PRECONDITION`; no Capsule removal/cloud teardown. A terminating child still counts until actually absent. |
| Management dependency read/CRD permission failure | Fail closed with an explicit reason; do not interpret unreadable children as an empty list. |
| Delete an allocation with probe workloads or other contents | Require-empty deletion reports/refuses blockers or stays finalizing; no caller-selectable force/orphan/reset option. Missing remote observation is not evidence of emptiness. |

A management freeze does **not** inherently stop Capsule's separate GTR
replication. Confirm how the deployed workload controller coordinates that
boundary before testing template rollout during a freeze. If no coordinated
Capsule pause mechanism exists, record this limitation instead of promising a
freeze stopped child updates. Do not edit a production shared GTR for this test.

## 9. Cleanup and record the result

Review the exact namespace names and fixture ownership before cleanup. Tenant A
and B remove only their lab workloads through their scoped identities:

```sh
ka -n "$A_APP" delete deployment,service subtenant-probe --ignore-not-found
ka -n "$A_WORKER" delete deployment,service subtenant-probe --ignore-not-found
kb -n "$B_APP" delete deployment,service subtenant-probe --ignore-not-found
ka -n "$A_APP" delete configmap subtenant-quota-one subtenant-quota-three --ignore-not-found
ka -n "$A_WORKER" delete configmap subtenant-quota-two subtenant-quota-three --ignore-not-found
# Remove any specifically named negative-test Deployment or B quota control too.
```

The operator then removes only `subtenant-test-workloads` and `subtenant-test-exec`
RoleBindings and the `subtenant-test-exec` Role from those three lab namespaces,
revokes the temporary identity/impersonation grant, and removes local rendered
fixtures/scoped credentials according to their issuance policy. Do not delete
generated baseline resources or namespaces directly, strip finalizers, disable
Capsule deletion protection, or bypass require-empty checks to accelerate cleanup.

Use the management UI's refreshed allocation → Delete review with the current
etag/typed ID. Wait for blocker reporting and controller-owned safe cleanup.
Before remote reconciliation exists, an unprovisioned intent can disappear
without deleting any workload namespace; this does not prove remote cleanup.
Only after both management children, including terminating ones, are gone may a
reviewed parent NaaS disable/delete proceed. Parent safety periodically retries;
the workload lifecycle implementation must independently demonstrate recovery
and finalizer behavior. Never infer cleanup from an accepted delete response.

Keep a result table with build/release, cluster/namespace UIDs, template revision,
identity and approved claims, positive and negative outcomes, exact denial
reasons, endpoint/node placements and sanitized flow evidence. Mark each stage
**pass**, **fail** or **blocked**. Configuration checks, lab-identity traffic
tests, actual user login, automatic qualification and access issuance are separate
results. This guide alone qualifies none of them and leaves access-ready false.
