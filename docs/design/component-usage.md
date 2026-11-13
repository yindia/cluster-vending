# Components: how a tenant enables one and overrides its values

Status: design, not implemented. Nothing below exists in code except where
a file or identifier is named.

This document locks the tenant-facing half of components: the objects, the
API and CLI, what is checked when a request arrives, how values are merged,
and Karpenter worked end to end. It builds on [components.md](components.md)
(the resource, the catalog, install mechanisms), [release-admin.md](release-admin.md)
(how an operator locks versions) and [rollouts.md](rollouts.md) (how
versions move). Where it refines a proposal of components.md it says so.

Every choice is marked **Decided (proposed)**: this document's decision, for
the project owner to confirm. Each names the alternative it rejects.

## 1. What is already decided (not reopened)

| Topic | Decision | Recorded in |
|---|---|---|
| Resource | Components are their own resource and their own API, not fields on the cluster. | components.md, section 2 |
| Opt-in | A new cluster has no components. | components.md, section 2 |
| Ownership | Account, network, cluster, component. An owner is never deleted while it has dependents: no cascade. | components.md, sections 2 and 12 |
| Releases | A `Release` lists only components the platform installs itself, each with a required version per Kubernetes minor. It never mentions EKS add-ons. | release-admin.md, section 3.1 |
| EKS add-ons | Stay in the cluster spec (`spec.aws.addons`), checked against the list in the contract (`AwsAddon.name`) and the kind's enum; AWS decides their versions. | `idl/cloud/v1/aws.proto`, `api/v1alpha1/cluster_types.go` |
| Install mechanism | Not decided for components: the Crossplane Helm provider is installed; Argo CD has no groundwork. | components.md, section 9 |
| Upgrade modes | Manual clusters get no version upgrades but do get platform fixes; automatic clusters follow their channel. | rollouts.md, section 4 |
| Overrides | Tenants pass configuration per component. | components.md, section 2 |
| References | `ReleaseHold` names a cluster by its cluster ID. | release-admin.md, section 3.3 |

## 2. What exists today

Checked against the code at the time of writing:

| Fact | Where |
|---|---|
| provider-helm v1.5.0 is installed with the AWS providers, and its namespaced `Release` kind (`helm.m.crossplane.io`) is activated. Nothing renders one. | `crossplane.HelmProvider`, `HelmProviderVersion` in `pkg/crossplane/providers.go`; `pkg/crossplane/aws/activation.go` |
| A forced cluster deletion already takes Helm `Release`s labelled with the cluster out of the way without uninstalling them. | `forceRelease` in `pkg/reconcile/cluster/cleanup.go` |
| Every cluster's `ClusterAuth` writes a kubeconfig Secret, `<prefix>-kubeconfig`, in the tenant namespace. | `clusterBuilder.clusterAuth`, `KubeconfigSecretName` in `pkg/crossplane/aws/cluster.go` |
| Add-ons that need AWS permissions get an IAM role, a policy attachment and a pod identity association for a fixed service account. | `addonSetups` in `pkg/crossplane/aws/addons.go` |
| A cluster tags the private subnets it uses with `kubernetes.io/cluster/<name>=shared` and, unless another cluster of the network has it, `karpenter.sh/discovery=<name>`. The cluster security group carries `aws:eks:cluster-name`, set by EKS. | `TagSubnets` in `pkg/reconcile/cluster/subnets.go`; `TagKarpenterDiscovery` in `pkg/crossplane/aws/network.go` |
| Access entries are rendered for admin principals and `spec.access.accessEntries`. | `accessEntries` in `pkg/crossplane/aws/cluster_options.go` |
| An OIDC identity provider of the Kubernetes API is rendered for `spec.access.oidc`. | `managedIdentityProvider` in `pkg/crossplane/aws/cluster_options.go` |
| The AWS provider families installed are ec2, eks, iam, cloudwatchlogs and kms. SQS and EventBridge are not. | `ProviderPackages` in `pkg/crossplane/aws/aws.go` |
| Every platform object a store keeps carries the label `cloud.cluster-vending.io/id` with its API ID. | `cloudstore.LabelID` |

## 3. The objects

### 3.1 Component (tenant-facing)

**Decided (proposed): a namespaced `Component` in the tenant namespace,
referring to its cluster by cluster ID.** Rejected: referring by name, as
components.md section 5 proposed with `clusterRef`. A cluster's ID is its API
identity, never reused, and is what `ReleaseHold` uses; a name is reused after
a delete. The cluster's name is shown in status for people.

**Decided (proposed): one `Component` per (cluster, component).** The object
is named `<cluster name>-<component>`; cluster names are unique per tenant,
so the name is unique in the namespace. Rejected: a list of components on
one object per cluster, which would make one broken component block every
other component's update.

```go
// ComponentSpec is what a tenant asks for: one component on one cluster.
// +kubebuilder:validation:XValidation:rule="self.clusterID == oldSelf.clusterID",message="clusterID is immutable"
// +kubebuilder:validation:XValidation:rule="self.component == oldSelf.component",message="component is immutable"
type ComponentSpec struct {
	// ClusterID is the cluster the component is installed into: the API's
	// Cluster.id. The controller sets the owner reference to that Cluster.
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`
	ClusterID string `json:"clusterID"`
	// Component is the ComponentDefinition name, e.g. karpenter.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`
	Component string `json:"component"`
	// Version pins a version on a manual cluster (section 7). Empty: the
	// version the release gives the cluster's Kubernetes minor.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	Version string `json:"version,omitempty"`
	// Values are the tenant's edits of the definition's userValues file
	// only (section 5). platformValues and computed values are never
	// copied here, and may not be set.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:validation:Type=object
	Values *apiextensionsv1.JSON `json:"values,omitempty"`
}

// ComponentStatus is written by the controller only.
type ComponentStatus struct {
	// Phase: Pending, Installing, Ready, Degraded, Failed, Deleting.
	Phase string `json:"phase,omitempty"`
	// Conditions: Valid (the request's checks hold now, section 4),
	// Synced (the platform's objects are applied), Ready (installed and
	// healthy as far as the install mechanism reports).
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	// Cluster is the cluster's name, resolved from spec.clusterID, for people.
	Cluster string `json:"cluster,omitempty"`
	// InstalledVersion is what runs; TargetVersion what it is moving to.
	InstalledVersion string `json:"installedVersion,omitempty"`
	TargetVersion    string `json:"targetVersion,omitempty"`
	// Release the version came from (empty for a pinned version).
	Release string `json:"release,omitempty"`
	// EffectiveValues are the merged values that were applied (section 5),
	// with platform-computed keys included and no secrets.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:validation:Type=object
	EffectiveValues *apiextensionsv1.JSON `json:"effectiveValues,omitempty"`
	// Inventory: the objects the platform has for this component, the same
	// shape as Cluster.status.inventory.
	Inventory []InventoryEntry `json:"inventory,omitempty"`
	Message   string           `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=cvcomp
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.status.cluster`
// +kubebuilder:printcolumn:name="Component",type=string,JSONPath=`.spec.component`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.status.installedVersion`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
type Component struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec   ComponentSpec   `json:"spec"`
	Status ComponentStatus `json:"status,omitempty"`
}
```

**Decided (proposed): effective values live in status.** They are bounded:
the tenant's part is capped at 64 KiB (section 5) and the platform's part is
small. Rejected: a hash plus a separate read RPC, which hides the one thing a
tenant needs when a value does not take effect.

### 3.2 ComponentDefinition (operator-owned catalog entry)

**Decided (proposed): cluster-scoped, written with `kubectl` or Git by the
`release-admin` role**, like `Release` (release-admin.md, section 4). Tenants
read it through `ListComponentDefinitions`; the install details are not part
of that answer.

The definitions this build ships are in `pkg/catalog/definitions/` and listed in
[catalog.md](../catalog.md). The controller creates the missing ones on
startup (`--install-catalog`) and never changes an existing one. The fields
`install.helm.crds`, `versionGroup` and `upgradeOneMinorAtATime` are
described in section 7.1; they are the operator's and not part of the
tenants' contract (`component.proto` has no install details).

```go
// ComponentDefinitionSpec is one entry of the catalog.
type ComponentDefinitionSpec struct {
	// Description shown to tenants.
	// +kubebuilder:validation:MaxLength=1024
	Description string `json:"description"`
	// Install is how the component reaches a workload cluster. Opaque to
	// tenants: the API never returns it.
	Install ComponentInstall `json:"install"`
	// Namespace in the workload cluster.
	Namespace string `json:"namespace"`
	// Requires: other components that must be Ready first (section 8).
	// +optional
	// +listType=set
	Requires []string `json:"requires,omitempty"`
	// ConflictsWithAddons: EKS add-on names the cluster must not have in
	// spec.aws.addons while this component is enabled (section 8).
	// +optional
	// +listType=set
	ConflictsWithAddons []string `json:"conflictsWithAddons,omitempty"`
	// CloudResources is what the platform creates in the tenant's account
	// before installing the component: an IAM role so far (section 5.2).
	// +optional
	CloudResources *ComponentCloudResources `json:"cloudResources,omitempty"`
	// PlatformValues are set by the platform team for every tenant and
	// locked: every leaf path in them wins, and a tenant who touches one
	// is refused. Tenants never see them in the values file.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:validation:Type=object
	PlatformValues *apiextensionsv1.JSON `json:"platformValues,omitempty"`
	// UserValues is the starting values file tenants pull (cluster-vending
	// component values <name>), edit and send with enable/update; only
	// keys a tenant may change. YAML in a string, so comments survive.
	// +optional
	// +kubebuilder:validation:MaxLength=65536
	UserValues string `json:"userValues,omitempty"`
	// ValuesSchema is a JSON Schema, written by hand by the platform team,
	// of what a tenant may override. Absent: no overrides are accepted.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:validation:Type=object
	ValuesSchema *apiextensionsv1.JSON `json:"valuesSchema,omitempty"`
}

// ComponentInstall: exactly one mechanism.
// +kubebuilder:validation:XValidation:rule="has(self.helm)",message="helm is the only install mechanism so far"
type ComponentInstall struct {
	Helm *HelmInstall `json:"helm,omitempty"`
}

// HelmInstall: a chart, and optional companion charts installed after it.
type HelmInstall struct {
	HelmChartSource `json:",inline"`
	// Companions are charts the platform owns, installed after the main
	// chart is Ready (e.g. Karpenter's default node class, section 6).
	// +optional
	Companions []HelmCompanion `json:"companions,omitempty"`
}

// HelmCompanion: name (its Helm release) plus a HelmChartSource.

// HelmChartSource is where a chart comes from. No version: the release
// pins it per Kubernetes minor (section 7).
// +kubebuilder:validation:XValidation:rule="!has(self.plainHTTP) || !self.plainHTTP || self.repository.startsWith('oci://')",message="plainHTTP is for oci:// registries"
type HelmChartSource struct {
	Repository string `json:"repository"` // ^(https|oci)://
	Chart      string `json:"chart"`
	// Credentials of a private repository; unset: public.
	Credentials *HelmCredentials `json:"credentials,omitempty"`
	// For test registries only.
	InsecureSkipTLSVerify bool `json:"insecureSkipTLSVerify,omitempty"`
	PlainHTTP             bool `json:"plainHTTP,omitempty"`
}

// HelmCredentials: a reference, never the secret. There is no field for
// a username, password or token.
type HelmCredentials struct {
	Type      HelmCredentialsType `json:"type"`      // BasicAuth | DockerConfigJSON
	SecretRef SecretReference     `json:"secretRef"` // namespace and name, both required
}
```

**Decided (with the user): private chart repositories are reached with a
Secret the operator creates in the platform namespace, referenced by the
definition.** `BasicAuth` is a `kubernetes.io/basic-auth` Secret (keys
`username`, `password`) for https repositories and OCI registries that take
basic auth; `DockerConfigJSON` is a `kubernetes.io/dockerconfigjson` Secret,
for OCI registries such as ECR whose pull secret a job outside the platform
refreshes. provider-helm v1.5.0 (`helm.m.crossplane.io/v1beta1` `Release`)
has one credential field, `spec.forProvider.chart.pullSecretRef`, a
`LocalSecretReference` to a Secret with `username` and `password` in the
Release's own (tenant) namespace. So the step that installs charts (step 6)
copies a `BasicAuth` Secret into the tenant namespace and, for
`DockerConfigJSON`, writes the username and password of the repository's
registry host from `.dockerconfigjson` into such a Secret; without
credentials provider-helm uses its default credential chain.
`insecureSkipTLSVerify` and `plainHTTP` map to the Release's
`spec.forProvider.insecureSkipTLSVerify` and `spec.forProvider.plainHTTP`.
That step also gives the controller `get` on Secrets in the platform
namespace only; step 1 adds no Secret access.

Print columns: `Requires`, `IAM` (the role's access, `IRSA` or `PodIdentity`), `Valid`.

**Decided (proposed): Kubernetes support comes from the Release only.** A
definition has no list of minors. A component is offered on a cluster when
the release that applies to the cluster lists it for the cluster's minor
(`Release.spec.content.kubernetes[].components`, release-admin.md section 3.1).
Rejected: a second list in the definition, which would let the definition and
the release disagree.

**Decided (proposed): components are never installed as EKS add-ons.** What
is available as an EKS add-on is offered through `spec.aws.addons`, versioned
by AWS; a component is always installed by the platform and versioned by the
release. Rejected: an `eksAddon` install mechanism (components.md section 6),
which would give one object two version sources and contradict the decision
that a release never mentions EKS add-ons. This resolves components.md open
question 9. When something exists both ways (cert-manager), the two are
mutually exclusive on a cluster (section 8).

**Decided (proposed): Helm, through provider-helm, is the install mechanism
the types carry now.** It is installed, the forced-delete path expects it,
and its `Release` objects go into the sequence engine like every other object.
The tenant-facing API does not depend on it: `install` is never returned to
tenants, and Argo CD can be added later as a second member of
`ComponentInstall` without changing `Component` or the RPCs. Rejected: Argo
CD now, which brings a second control plane to run and a second system that
decides when something is done (components.md, section 9).

## 4. How a tenant enables one

### 4.1 The API

A new `ComponentService` in `idl/cloud/v1/component_service.proto`, additive:

```protobuf
service ComponentService {
  rpc ListComponentDefinitions(ListComponentDefinitionsRequest) returns (ListComponentDefinitionsResponse) {
    option idempotency_level = NO_SIDE_EFFECTS;
  }
  rpc EnableComponent(EnableComponentRequest) returns (EnableComponentResponse);
  rpc UpdateComponent(UpdateComponentRequest) returns (UpdateComponentResponse);
  rpc DisableComponent(DisableComponentRequest) returns (DisableComponentResponse);
  rpc GetComponent(GetComponentRequest) returns (GetComponentResponse) {
    option idempotency_level = NO_SIDE_EFFECTS;
  }
  rpc ListComponents(ListComponentsRequest) returns (ListComponentsResponse) {
    option idempotency_level = NO_SIDE_EFFECTS;
  }
}

message Component {
  string tenant_id = 1;
  string id = 2;                         // server-assigned
  string cluster_id = 3;                 // required, immutable
  string component = 4;                  // definition name, required, immutable
  string version = 5;                    // optional pin, manual clusters only
  google.protobuf.Struct values = 6;     // the tenant's overrides
  ComponentStatus status = 7;            // output only
  string etag = 8;
  int64 generation = 9;
}

message EnableComponentRequest {
  string tenant_id = 1;
  Component component = 2;
  string request_id = 3;                 // idempotency, as for clusters
  bool validate_only = 4;                // answer with effective values, store nothing
}

message UpdateComponentRequest {
  string tenant_id = 1;
  Component component = 2;
  google.protobuf.FieldMask update_mask = 3;   // "values", "version"
  bool validate_only = 4;
}

message DisableComponentRequest {
  string tenant_id = 1;
  string component_id = 2;
  string etag = 3;
  bool force = 4;                        // section 6.5 (Karpenter's nodes)
}

message ListComponentsRequest {
  string tenant_id = 1;
  string cluster_id = 2;                 // optional filter
  int32 page_size = 3;
  string page_token = 4;
}

message ComponentDefinition {           // what tenants see of a catalog entry
  string name = 1;
  string description = 2;
  repeated string requires = 3;
  repeated string conflicts_with_addons = 4;
  google.protobuf.Struct values_schema = 5;
  reserved 6; reserved "protected_keys";    // published, then replaced
  google.protobuf.Struct platform_values = 8;  // locked for tenants
  // versions offered, per Kubernetes minor, by the cluster's release
  map<string, string> versions = 7;
  string user_values = 9;                // the values file, YAML with comments
}

rpc GetComponentValues(GetComponentValuesRequest{string name})
    returns (GetComponentValuesResponse{string user_values});  // NO_SIDE_EFFECTS
```

`ListComponentDefinitionsRequest` takes an optional `cluster_id`; with it,
`versions` holds what that cluster's release offers for its minor.

**Decided (proposed): disable is a delete, not an update to "disabled".**
Rejected: an `enabled` flag, which would leave stopped components and their
cloud resources around and blur what the tenant pays for.

### 4.2 The CLI

```sh
cluster-vending component catalog [--cluster <cluster-id>]
cluster-vending component values karpenter > karpenter.yaml
cluster-vending component enable karpenter --cluster <cluster-id> [-f values.yaml] [--validate-only]
cluster-vending component update <component-id> -f values.yaml --update-mask values [--validate-only]
cluster-vending component update <component-id> --version 1.8.1 --update-mask version
cluster-vending component disable <component-id> [--wait] [--force]
cluster-vending component get <component-id>
cluster-vending component list --cluster <cluster-id>
```

`-f` takes the values only, as YAML or JSON. `--validate-only` prints the
effective values and every refused key without storing anything.

**The values file.** A definition carries a values file
(`ComponentDefinition.spec.userValues`, YAML with the platform team's
comments, only the keys a tenant may change). A tenant starts from it:

```sh
cluster-vending component values karpenter > karpenter.yaml   # GetComponentValues
$EDITOR karpenter.yaml
cluster-vending component enable karpenter --cluster <cluster-id> -f karpenter.yaml
```

The definition is `Valid` only when its file parses as YAML into an object,
shares no path with `platformValues` (the same leaf, or one inside the
other) and validates against `valuesSchema` (`pkg/component`
`CheckDefinition`), so the file as pulled is always accepted.
`platformValues` are what every tenant gets, locked, without seeing them in
the file.

### 4.3 The same through kubectl

```yaml
apiVersion: cloud.cluster-vending.io/v1alpha1
kind: Component
metadata:
  name: prod-eks-karpenter
  namespace: tenant-acme
spec:
  clusterID: 01a1d0a6-69d5-7e15-965e-fc6a53fa3d33
  component: karpenter
  values:
    replicas: 2
```

A `Component` applied with `kubectl` is adopted by the controller the way
hand-created networks and clusters are (`pkg/controller`): it runs the same
checks as the API (section 4.4) and, if one fails, sets `Valid=False` with the
reason instead of installing. The CRD's own rules catch only the shape.

### 4.4 What is checked when the request arrives

All of it reads the store and the release objects; nothing calls AWS
(the API server has no AWS access).

| # | Check | Answer when it fails |
|---|---|---|
| 1 | The definition exists and is `Valid`. | `NOT_FOUND`: "component karpenter is not in the catalog" |
| 2 | The cluster exists in the tenant, is not being deleted, and is not imported without takeover. | `NOT_FOUND` / `FAILED_PRECONDITION`: "take the cluster over first" |
| 3 | The cluster is `RUNNING`. | `FAILED_PRECONDITION`: "the cluster is PROVISIONING; enable components once it is RUNNING" |
| 4 | The component is not already enabled on the cluster. | `ALREADY_EXISTS` |
| 5 | The release that applies to the cluster (section 7) lists the component for the cluster's Kubernetes minor. | `FAILED_PRECONDITION`: "karpenter is not offered on Kubernetes 1.35 in release 2026-11-a" |
| 6 | A pinned version is allowed (section 7). | `INVALID_ARGUMENT` / `FAILED_PRECONDITION` |
| 7 | Every `requires` entry is enabled on the cluster. | `FAILED_PRECONDITION`: "enable kyverno first" |
| 8 | No `conflictsWithAddons` entry is in the cluster's `spec.aws.addons`. | `FAILED_PRECONDITION`: "remove the cert-manager add-on first" |
| 9 | The values parse, are an object, are at most 64 KiB. | `INVALID_ARGUMENT` |
| 10 | No value touches a `platformValues` path (or, later, a computed one): the same leaf, a path inside it, or one above it (section 5). | `INVALID_ARGUMENT`, naming every such path |
| 11 | The values validate against `valuesSchema`. | `INVALID_ARGUMENT`, with the schema's error and path |
| 12 | The cluster is supported (its minor is in the release). | `FAILED_PRECONDITION` (rollouts.md: enabling on an unsupported cluster is refused) |

Before these checks, `EnableComponent` and `UpdateComponent` call
`component.PruneUnchanged(sent, file)` with the definition's `userValues` file:
every key whose value equals the file's at the same path is dropped
(maps key by key, recursively; lists whole), and the checks and the store
see only what is left. So only real edits are stored, and a later change to
the file reaches every tenant who did not touch that key. **The tradeoff:**
a tenant cannot pin a value that equals today's file; when the file changes,
so does the value. To keep a value whatever the file says later, the
platform has to move it to a key the file does not carry.

Then the handler stores the component, and the controller installs it
(write-only handlers, as for networks and clusters).

The reverse checks also run in the API: `UpdateCluster` refuses adding an add-on
named in an enabled component's `conflictsWithAddons`, and `DeleteCluster`
refuses while the cluster has components (section 9).

## 5. Values

**Decided (proposed): the format is a JSON object** (`google.protobuf.Struct`
on the contract, schemaless JSON on the kind; the CLI reads YAML), at most
64 KiB serialized. **Values are not for secrets**: a component that needs one
refers to a Secret in the workload cluster by name.

**Decided (proposed): the merge order, later wins**:

| # | Layer | Who sets it | Changes when |
|---|---|---|---|
| 0 | The chart's own defaults (under everything, inside Helm) | the chart's authors | the version changes |
| 1 | `ComponentDefinition.spec.userValues`, the values file | platform team | the definition is edited |
| 2 | `Component.spec.values`, the tenant's edits of that file | tenant | `UpdateComponent` |
| 3 | `ComponentDefinition.spec.platformValues`, locked | platform team | the definition is edited |
| 4 | Placeholders in `platformValues` filled in per cluster (`${cluster.name}`, `${iam.roleArn}`, ...; section 5.1) | the controller | the cluster or the component's role changes |

`component.Merge(chart, userValues, overrides, platformValues)` merges
layers 0 to 3. Layers 3 and 4 always win, but not silently: a request
whose values touch a locked path is refused (check 10), so a stored
component never carries one through the API. `valuesSchema` stays written by
hand; the platform does not derive it. The release adds no values layer: it pins versions only
(release-admin.md, section 3.1). Rejected: release-level defaults per minor
(components.md section 7 step 2), which would make a release change behaviour
as well as versions and complicate "what changed in this release".

**Decided (proposed): maps merge key by key; lists replace as a whole; an
explicit `null` removes a default.** This is Helm's own behaviour, so what a
tenant reads in the chart's documentation holds. A `null` on a locked path
is refused like any other value on it.

**Locked paths** are every leaf path of `platformValues` (and later the
computed values), dotted (`controller.resources.requests.cpu`). A tenant
value at the path or below it is refused, and so is a value above it that
would replace it (`controller: null`). Every refused path is named in one
answer. A definition whose `userValues` share a path with its
`platformValues` is not `Valid`.

**Seeing the result.** `--validate-only` returns the effective values and the
refused keys without storing. After an apply, `status.effectiveValues` is
what was given to the install object.

**Rollout of a values change.** In place: the controller re-renders the
install object, and provider-helm runs an upgrade with the new values. It does
not wait for a release or a tier, on manual or automatic clusters: values are
the tenant's own configuration, not a platform version. A change to the
definition's `platformValues`, or of a key of its `userValues` file a tenant
did not edit, is a platform change and reaches every cluster, manual ones
included (manual protects versions, not configuration fixes).

### 5.1 Placeholders: values the platform fills in

The platform team writes `${name}` in a definition's `platformValues` and in
its IAM policy (`cloudResources.iamRole.policy`, `managedPolicyArns`); the
controller fills them in per cluster before it installs the component. The
list is fixed in code (`pkg/component`, `PlaceholderNames`): each name has one
source, and a definition naming any other is invalid (`Valid=False`, the
name and its path in the message).

| Placeholder | Source | Known once |
|---|---|---|
| `${region}` | Cluster `spec.region` | always |
| `${account.id}` | the owning CloudAccount `spec.aws.accountID` | always |
| `${cluster.name}` | the Cluster's name (the EKS cluster's name) | always |
| `${cluster.endpoint}` | Cluster `status.endpoint.url` | the control plane is up |
| `${cluster.oidcIssuer}` | Cluster `status.oidcIssuerURL` | the control plane is up |
| `${cluster.oidcProviderArn}` | `arn:<partition>:iam::<account>:oidc-provider/<issuer without https://>` | the issuer is known, on a cluster with `spec.aws.irsa: true` only; with IRSA off it never is, and the wait says so |
| `${cluster.nodeRoleName}` | the IAM name of the cluster's managed node group role: the name an import recorded, or `<prefix>-node-role` | always |
| `${cluster.nodeRoleArn}` | `arn:<partition>:iam::<account>:role/<nodeRoleName>` | always |
| `${network.vpcId}` | the owning Network `status.resources.cloudNetworkID` | the network is built |
| `${iam.roleArn}`, `${iam.roleName}` | the component's own role (section 5.2), from its status | the role exists |
| `${iam.policyArn}` | the component's own policy | the policy exists |
| `${system.nodeSelector}` | the labels of the cluster's default node pool (`spec.defaultNodePool.labels`), as a map; `{}` for none | always |
| `${system.tolerations}` | one toleration per taint of the default node pool (`spec.defaultNodePool.taints`): `{key, operator: Equal, value, effect}`; `[]` for none | always |

Rules:

- A placeholder may sit inside a longer string
  (`arn:aws:iam::${account.id}:policy/x`). `$${` writes a literal `${`.
- The two `system.*` names are **whole values**: the field's value must be
  exactly the placeholder (`nodeSelector: ${system.nodeSelector}`), and it is
  replaced by the map or the list. Inside a longer string they are refused,
  with the path. They are for `platformValues` only, not the IAM policy.
- A known name without a value yet is not an error: the controller waits and
  tries again (`ErrNotReady`, naming each missing value). It never installs a
  blank.
- The `iam.*` names are not available in the IAM policy and managed policy
  ARNs: they name the role and policy that policy belongs to.
- **Tenant values are taken literally and never expanded.** A tenant cannot
  read another account's values through a placeholder.
- A value changes (a new endpoint): the controller fills it in again and
  provider-helm upgrades the release.

#### Running on the default node pool

The cluster's default node pool is where the platform's components run. A
tenant who wants it for nothing else gives it labels and a taint
(`spec.defaultNodePool.labels` and `.taints`); the platform team makes every
catalog component follow it, at the chart's own keys:

```yaml
platformValues:
  nodeSelector: ${system.nodeSelector}
  tolerations: ${system.tolerations}
  # a chart with several workloads repeats it per workload, e.g.
  # controller: {nodeSelector: ${system.nodeSelector}, tolerations: ${system.tolerations}}
```

- A list replaces the chart's own default as a whole. A chart whose default
  tolerations matter keeps them only if the definition does not set the key:
  such a component sets the nodeSelector and leaves the tolerations alone, or
  the platform team checks that the pool's tolerations are enough.
- DaemonSets that must run on every node (a CNI, ztunnel, a device plugin)
  do not take the nodeSelector; they tolerate every taint in their own chart.
- The same placement is applied to the EKS add-ons whose configuration
  schema is confirmed (coredns and the EBS CSI controller; the table is in
  `pkg/crossplane/aws/addon_placement.go`). A nodeSelector or tolerations
  the tenant set in the add-on's `configurationValues` wins. The other
  add-ons' controllers are placed by the tenant, the same way.
- A default pool with a `NoSchedule` or `NoExecute` taint leaves tenant
  workloads nowhere to run until other nodes exist (the karpenter component)
  or they tolerate the taint. The API does not refuse it: the plan of a
  cluster update carries a warning.

### 5.2 Cloud resources: an IAM role

A definition may ask for one IAM role per cluster it is enabled on. Only an
IAM role and its policies can be declared so far; no queues or other
presets (Karpenter's interruption queue comes later).

```yaml
cloudResources:
  iamRole:
    access: IRSA            # default; or PodIdentity
    serviceAccounts:        # 1 to 8
      - {namespace: kube-system, name: aws-load-balancer-controller}
    policy: |               # optional; placeholders allowed, iam.* excepted
      {"Version": "2012-10-17", "Statement": [...]}
    managedPolicyArns:      # optional, at most 10 with the policy
      - arn:aws:iam::${account.id}:policy/AWSLoadBalancerControllerIAMPolicy
```

**The trust policy is always the platform's**, never written in the
definition:

| Access | Trust policy | Also created |
|---|---|---|
| `IRSA` (default) | `Federated` = the cluster's IAM OIDC provider, `sts:AssumeRoleWithWebIdentity`, `<issuer>:aud` = `sts.amazonaws.com` and `<issuer>:sub` = each `system:serviceaccount:<ns>:<name>` | nothing; the role is built once the cluster's OIDC provider has its ARN |
| `PodIdentity` | `pods.eks.amazonaws.com`, `sts:AssumeRole` and `sts:TagSession` | one pod identity association per service account |

**IRSA needs the cluster's OIDC provider.** Enabling a component whose role
uses IRSA on a cluster with `spec.aws.irsa` off is refused with
`FAILED_PRECONDITION`, naming the setting (`component.CheckCluster`).

**The service account annotation is the platform team's to write.** The
platform does not annotate service accounts: each chart has its own key. For
IRSA the definition's `platformValues` set it with `${iam.roleArn}`:

```yaml
platformValues:
  serviceAccount:
    annotations:
      eks.amazonaws.com/role-arn: "${iam.roleArn}"
```

**Names.** Every object is named `<cluster prefix>-<component>-...`, cut and
hashed to fit IAM's 64 characters: the role `...-role`, the policy
`...-policy`, attachments `...-attach-policy` and `...-attach-<n>`,
associations `...-identity-<n>`. They carry the cluster's labels and
`cluster-vending.io/component`.

**Order** (`crossplaneaws.ComponentIAMSet`, one set per component):

1. The policy and the role (an IRSA role once the OIDC provider is known).
2. The attachments and associations, once the role (and the policy) is
   Ready.
3. Placeholders filled in (section 5.1), then the Helm release.

Deletion is the reverse, held by Usages: the release goes first, then the
associations, then the attachments, then the role and the policy.

## 6. Karpenter, end to end

> **Later.** This section is the target for Karpenter. What can be declared
> today is an IAM role and policy (section 5.2); the interruption queue, its
> EventBridge rules, the node role and access entry below need a Karpenter
> preset that does not exist yet. The sample definition in
> `config/samples` uses an IRSA role with `iam:PassRole` on
> `${cluster.nodeRoleArn}` (the cluster's own node role) until then.

### 6.1 The definition

```yaml
apiVersion: cloud.cluster-vending.io/v1alpha1
kind: ComponentDefinition
metadata:
  name: karpenter
spec:
  description: Node autoscaling. Launches EC2 instances for pending pods.
  namespace: kube-system
  cloudResources:                         # section 5.2; a Karpenter preset later
    iamRole: {serviceAccounts: [{namespace: kube-system, name: karpenter}], policy: '...'}
  install:
    helm:
      repository: oci://public.ecr.aws/karpenter
      chart: karpenter
      companions:
        - name: karpenter-defaults          # platform chart: EC2NodeClass "default" (+ optional NodePool)
          repository: oci://<platform registry>/charts
          chart: karpenter-defaults
  platformValues:                         # locked; settings.clusterName, clusterEndpoint,
    controller:                           # interruptionQueue, serviceAccount and
      resources: {requests: {cpu: 250m, memory: 512Mi}}   # defaultNodeClass are computed and locked too
  userValues: |
    # Controller replicas, 1 to 3. Two keep Karpenter up while its node drains.
    replicas: 2
    # error, info or debug.
    logLevel: info
    settings:
      # How long Karpenter collects pending pods before it launches nodes.
      batchMaxDuration: 10s
    defaultNodePool:
      # false: no default NodePool; bring your own.
      enabled: true
      # The most CPU the default NodePool may launch.
      limits: {cpu: "64"}
      # Replaced as a whole when you set it. Add spot to save money.
      requirements:
        - {key: karpenter.sh/capacity-type, operator: In, values: [on-demand]}
  valuesSchema:
    type: object
    additionalProperties: false
    properties:
      replicas: {type: integer, minimum: 1, maximum: 3}
      logLevel: {type: string, enum: [error, info, debug]}
      settings:
        type: object
        properties:
          featureGates: {type: object, additionalProperties: {type: boolean}}
          batchMaxDuration: {type: string}
      defaultNodePool:
        type: object
        properties:
          enabled: {type: boolean}
          limits: {type: object}
          requirements: {type: array}
          disruption: {type: object}
```

### 6.2 Cloud prerequisites a Karpenter preset would create (later)

| Object | Purpose | Exists today? |
|---|---|---|
| IAM role `<prefix>-karpenter` + policy scoped to the cluster | The controller's permissions (EC2 fleet, pricing, SSM, SQS, `iam:PassRole` on the node role, instance-profile management). | No (new renderer) |
| Pod identity association `kube-system/karpenter` → that role | Gives the controller its role, as add-ons get theirs. | Pattern exists (`addonSetups`) |
| IAM role `<prefix>-karpenter-node` with the worker, CNI and ECR policies | The role of nodes Karpenter launches. Karpenter v1 creates the instance profile from it (`EC2NodeClass.spec.role`), so the platform renders no instance profile. | No |
| EKS access entry, type `EC2_LINUX`, for the node role | Lets those nodes join under API authentication. | Access entries exist; this type is new |
| SQS queue `<prefix>-karpenter` + queue policy | Interruption messages. | No; needs the SQS provider family |
| EventBridge rules (spot interruption, rebalance, instance state change, scheduled change) targeting the queue | Feed the queue. | No; needs the CloudWatch Events provider family |
| Subnet tag `karpenter.sh/discovery=<cluster>` | Subnet discovery. | Yes (`TagSubnets`), except when another cluster of the network already holds the tag; the node class then selects `kubernetes.io/cluster/<name>` |
| Security group discovery | The cluster security group, by EKS's own tag `aws:eks:cluster-name`. | Yes (EKS sets it) |

Adding SQS and CloudWatch Events follows the rule for new provider families:
the install path (`ProviderPackages`), the activation policy and the cache
list, all three.

### 6.3 A tenant's request

```yaml
# karpenter.yaml: from `cluster-vending component values karpenter`, edited
replicas: 2
logLevel: info
settings:
  batchMaxDuration: 10s
  featureGates: {spotToSpotConsolidation: true}
defaultNodePool:
  limits: {cpu: "128"}
  requirements:
    - {key: karpenter.sh/capacity-type, operator: In, values: [spot, on-demand]}
    - {key: node.kubernetes.io/instance-type, operator: In, values: [m6i.large, m6i.xlarge]}
```

```sh
cluster-vending component enable karpenter --cluster 01a1d0a6-69d5-7e15-965e-fc6a53fa3d33 -f karpenter.yaml --validate-only
cluster-vending component enable karpenter --cluster 01a1d0a6-69d5-7e15-965e-fc6a53fa3d33 -f karpenter.yaml
```

Of that file only `settings.featureGates` and `defaultNodePool` (`limits`,
`requirements`) differ from `userValues`; they are what is stored in
`Component.spec.values`.

Effective values after the merge (what lands in `status.effectiveValues`):

```yaml
replicas: 2
logLevel: info
controller:                                  # platformValues, locked
  resources: {requests: {cpu: 250m, memory: 512Mi}}
settings:
  batchMaxDuration: 10s                      # userValues file
  clusterName: prod-eks                      # computed, locked
  clusterEndpoint: https://<eks endpoint>    # computed (late value), locked
  interruptionQueue: prod-eks-8f819a1f-karpenter   # computed, locked
  featureGates: {spotToSpotConsolidation: true}
serviceAccount: {name: karpenter}            # computed, locked (pod identity names it)
defaultNodeClass:                            # computed, locked; given to the companion chart
  role: prod-eks-8f819a1f-karpenter-node
  subnetSelector: {karpenter.sh/discovery: prod-eks}
  securityGroupSelector: {aws:eks:cluster-name: prod-eks}
defaultNodePool:
  enabled: true
  limits: {cpu: "128"}                       # tenant, replaces the default map key
  requirements:                              # tenant, replaces the default list
    - {key: karpenter.sh/capacity-type, operator: In, values: [spot, on-demand]}
    - {key: node.kubernetes.io/instance-type, operator: In, values: [m6i.large, m6i.xlarge]}
```

### 6.4 NodePools and EC2NodeClasses

**Decided (proposed):** the component ships one `EC2NodeClass` named
`default`, with the locked role and selectors above, and optionally one
`NodePool` named `default` driven by `defaultNodePool` values, both through
the platform's companion chart. Any further NodePool or EC2NodeClass is the
tenant's own object, applied inside their cluster with `kubectl`, and may
refer to the `default` class. Rejected: every NodePool as component values,
which would turn the platform into a NodePool API (a non-goal, components.md
section 4); rejected: no default at all, which leaves tenants to find the node
role and selectors the platform created.

### 6.5 What the platform creates, in order

One `sequence.Set` per component, applied by the engine:

| Step | Objects | Waits for |
|---|---|---|
| 1 | Controller role and policy, node role and attachments, SQS queue and policy, EventBridge rules and targets | nothing |
| 2 | Pod identity association, node access entry | the EKS cluster ACTIVE (`Field` condition, as add-ons) |
| 3 | Helm `Release` karpenter | step 2 Ready; built late with the queue name and cluster endpoint (`Item.Build`) |
| 4 | Helm `Release` karpenter-defaults | the karpenter `Release` Ready (its CRDs exist) |

Usages order the deletion: the roles and queue are used by the karpenter
`Release`, and the defaults `Release` by the karpenter one.

**Disable.** Reverse order, with one extra wait. The defaults `Release` is
deleted first, which deletes the default NodePool; Karpenter then drains and
terminates its nodes. The controller waits until no EC2 instance tagged
`karpenter.sh/nodepool` and `kubernetes.io/cluster/<name>` remains in the
account (one `DescribeInstances` call through the controller's existing AWS
access; the workload cluster's API is not needed), then deletes the karpenter
`Release`, then the cloud resources. The wait times out after 30 minutes:
the component shows `Phase=Failed`, reason `NodesRemain`, with the instance
IDs. `DisableComponent` with `force` skips the wait and terminates the
remaining instances carrying both tags, then continues. Rejected: removing
Karpenter while its nodes run, which leaves instances without a controller,
billed, and blocks deleting the node role.

## 7. Versions, releases, modes and holds

| Situation | The component's version |
|---|---|
| Automatic cluster | The version the cluster's release lists for its minor. It moves when the cluster's release target moves, in the cluster's (channel, tier) stage, after the control plane, node groups and add-ons. A pin is refused (`INVALID_ARGUMENT`). |
| Automatic cluster, held | What the held release lists; frozen if the hold names none. |
| Manual cluster, no pin | The version the release that applies to it lists for its minor at the moment it is enabled (release-admin.md, section 5: the newest release's pins), then fixed. It changes only when the tenant updates the component or the cluster's minor. |
| Manual cluster, pin | Allowed if any `Valid`, not withdrawn release lists that version for the cluster's minor. Anything else is refused, so nothing untested by a release is installed. |
| Manual cluster changes its Kubernetes minor | Unpinned components move to the version listed for the new minor after the control plane reports it, as add-ons do (`GateUpgrade`). A pinned component whose version is not listed for the new minor blocks the cluster update, naming the component. |
| Platform fix (renderer, or the definition's platformValues or userValues) | Reaches every cluster, manual included; versions do not change. |
| Any cluster outside the support window | Installed components stay; enabling new ones is refused. |

The tier and channel are the cluster's; a component has none of its own.

### 7.1 CRD charts, version groups and upgrade order

**CRD charts (built: the field and its checks; the installer comes with
step 6).** Helm installs a chart's `crds/` directory once and never upgrades
it. A definition may name a chart that holds only the component's CRDs as
templates:

```yaml
install:
  helm:
    repository: oci://public.ecr.aws/karpenter
    chart: karpenter
    crds:
      repository: oci://public.ecr.aws/karpenter
      chart: karpenter-crd
```

- The CRD chart is installed, and upgraded, **before** the main chart, at
  the version the release pins for the component (`ComponentPin.version`;
  there is no separate pin: upstream CRD charts are released with their
  main chart, and the platform's own follow the upstream version).
- The main chart then runs with provider-helm's
  `spec.forProvider.skipCRDs: true` (present in the provider-helm v1.5.0
  `Release` CRD).
- It gets no values and may carry `credentials` like any chart.
- No shipped definition uses one: each installs its one upstream chart. See [catalog.md](../catalog.md).

**Version groups (built).** `versionGroup: <name>` on a definition marks
components that are versions of one product (the Istio charts; `kyverno` and
`kyverno-policies`). A `Release` that pins members of a group at different
versions on the same Kubernetes minor gets `Valid=False`
(`release.CheckGroups`, in the release controller). A group partly pinned is
fine.

**One minor at a time (built).** `upgradeOneMinorAtATime: true` (Istio:
in-place upgrades skip no minor). A channel does not move to a release whose
pin skips a minor version of such a component relative to the release the
channel offers now: `Offering=False`, naming the component and both
versions, and the channel keeps offering what it did (`release.CheckStep`,
in the channel controller). A cluster moving to a new Kubernetes minor is
compared with the highest version the current release pins for the
component. The operator publishes the in-between release first.

**Upgrade order (built as a pure function; the installer must follow it).**
Components are installed and upgraded in `requires` order, then by name
(`release.UpgradeOrder`): `istio-base`, `istiod`, `istio-cni`, `ztunnel`;
`kyverno` before `kyverno-policies`. A cycle, or a requirement the catalog
does not have, is an error.

**Values a release sets (built).** A pin may carry `values`
(`ComponentPin.values`): Helm values for the component on that Kubernetes
minor, merged over the definition's `platformValues`
(`component.WithPinValues`), locked the same way, placeholders allowed.
The cluster autoscaler's image tag is set this way, since its minor must
equal the cluster's.

### 7.2 Platform components and bundles

**Who controls a component (built: the field and the pure checks).**
`spec.management` is `Tenant` (the default) or `Platform`.

- **Tenant** components are what this document describes: a tenant enables
  one on a cluster, which creates a `Component`. Shipped: `gpu-operator`
  and `kyverno-policies`.
- **Platform** components are part of cluster creation. No `Component`
  object exists for them and they never appear in `ListComponents`.
  `EnableComponent`, `UpdateComponent` and `DisableComponent` refuse their
  names with `PERMISSION_DENIED` ("installed with the cluster";
  `component.CheckTenantAccess`, used by `CheckEnable` and `CheckDisable`).
  A Platform definition has no `userValues` or `valuesSchema`
  (`CheckDefinition`) and requires only Platform definitions
  (`CheckCatalog`). A Tenant component may require a Platform one; nothing
  has to be enabled first for it (`Request.Catalog`).
- `ListComponentDefinitions` (handler: later) shows only Tenant, not
  internal definitions (`component.TenantCatalog`).

**How Platform components are installed (built).** The cluster's
reconcile installs them after its own pass, as a second set of objects
recorded apart from the cluster's (`status.componentInventory`, owner
`cluster_components`), so the cluster's state never waits for them:

1. **When.** Not before the cluster is RUNNING: its node group is Ready and
   its kubeconfig exists. Every chart also waits for the `<prefix>-helm`
   ProviderConfig and the node group by itself (`crossplaneaws.PlatformSet`).
   Once started, installing goes on through updates of the cluster. Nothing
   is installed on an imported cluster, also after its takeover (later).
2. **Which release.** The release the cluster's channel offers when its
   components are first installed (`spec.releaseChannel`, `stable` when
   unset), recorded as `status.platformRelease` and kept from then on, for
   manual and automatic clusters alike; moving a cluster to another release
   is the rollout step, later. "Installed" means an object of a component
   exists (a role or a chart): until then every pass follows the channel,
   so a release that pinned nothing, or one looked at before the kubeconfig
   existed, does not tie the cluster to it. Versions are that release's pins on the
   cluster's Kubernetes minor. A component the release does not pin is not
   installed ("not in release <name>"), and neither is anything requiring
   it. A companion chart is pinned under its own name;
   without a pin it alone is left out.
3. **What, per component** (`component.PlanPlatform`, then
   `crossplaneaws.PlatformSet`), each step once the one before is Ready:
   its IAM role (section 5.2), its CRD chart (Helm release
   `<component>-crds`), its chart (with `skipCRDs` when it has a CRD
   chart), its companions in order. A component starts after the last
   chart of every component it requires. Bundles are expanded to their
   members (`component.PlatformComponents`).
4. **Values.** The definition's `platformValues` with the pin's values
   merged over them, placeholders filled in (section 5.1) from the
   cluster's recorded status (endpoint, issuer), its network and account,
   the default node pool, and the component's own role and policy as the
   management cluster has them. A placeholder without a value yet (the
   role's ARN before the role exists) keeps the component's charts back,
   and its status says which. The key named after a companion goes to that
   companion only.
5. **The object.** A provider-helm `Release` (`helm.m.crossplane.io/v1beta1`,
   unstructured) in the tenant namespace, named `<prefix>-<helm release>`:
   `crossplane.io/external-name` is the Helm release name (the component's,
   so a chart renders as upstream documents it), `spec.providerConfigRef`
   the cluster's ProviderConfig, `spec.rollbackLimit` 100 (a chart that
   cannot install yet is retried, not given up),
   `spec.forProvider.chart.{repository,name,version}`, `.namespace` (the
   definition's; provider-helm creates it), `.values`, `.skipCRDs`. Only
   public chart repositories: a Platform definition with `credentials` is
   reported as not installable (private charts: later).
6. **Ready.** A chart counts as installed when its Release is Ready and
   `status.atProvider.state` is `deployed`. provider-helm reports that once
   Helm applied the chart, before its workloads run; workload checks are
   later.
7. **Status.** `Cluster.status.platformComponents` lists each one with its
   pinned version, a phase (`Pending`: waits, with what for; `Installing`;
   `Ready`; `Failed`: an install failed, or it cannot be installed on this
   cluster) and a message, and the condition `PlatformComponentsReady` sums
   them up. While it is `Installing` the controller looks at the cluster
   again every 20 seconds although it is RUNNING. The cluster's own phase
   and Ready do not depend on any of it. `ListComponents` is to return them
   too, read-only (`Component.management = PLATFORM`, no id or etag;
   `component.Installed` merges them with the tenant's own; handlers:
   later).
8. **Deletion.** Before anything of the cluster itself: every chart is
   uninstalled, a chart before the one it was installed after and a
   component before what it requires (Usages), then the roles. Uninstalling
   first lets the load balancer controller and Karpenter remove what they
   created in the account while they still run. When the workload cluster
   cannot be reached anyway (its EKS cluster is not ACTIVE, or the deletion
   is forced), the Releases are let go without an uninstall
   (`forceRelease`: `managementPolicies` without `Delete`), so a deletion
   never waits for a cluster that is not there. A cluster that is ACTIVE
   but unreachable (its endpoint closed off) needs `force`.

**Bundles (built: the fields and the pure checks).** A definition with
`includes` and no `install` is one component made of several charts. Each
member is a definition of its own, marked `internal: true`, with its chart,
namespace, version pin, `versionGroup` and `requires`.

- Enabling the bundle installs every member in `requires` order
  (`component.BundleOrder`), each once the one before is Ready; disabling
  removes them in reverse order; the bundle is Ready when every member is.
  A bundle's status reports each member's phase.
- A release pins every member of a bundle on a Kubernetes minor, or none
  (`release.CheckBundles`, in the release controller's Valid condition).
- An internal definition cannot be enabled by itself: the refusal names the
  bundle. It is a member of exactly one bundle, and bundles do not nest
  (`CheckCatalog`; the CRD refuses an internal bundle and a definition with
  both `install` and `includes`).
- Shipped: `istio` (Platform), six members.

## 8. Dependencies and conflicts

**kyverno and kyverno-policies.** `kyverno-policies` has `requires: [kyverno]`.

- Enabling `kyverno-policies` alone: refused, "enable kyverno first".
- Its `Release` waits for the kyverno `Release` to be Ready (a `Ready`
  condition across the two components' sets).
- Disabling `kyverno` while `kyverno-policies` is enabled: refused, naming it.

**cert-manager as an add-on and as a component.** cert-manager exists as an
EKS add-on (in `spec.aws.addons`, version from AWS, AWS's configuration
schema) and can be a platform component (Helm, version from the release, the
chart's full values). Two installations in one cluster would fight over the
same CRDs.

**Decided (proposed):** the `cert-manager` definition has
`conflictsWithAddons: [cert-manager]`. The API refuses enabling the component
while the add-on is in the cluster spec, and refuses adding the add-on while
the component is enabled; the controller reports `Valid=False` if objects
created with `kubectl` get around the API. Moving from one to the other is
remove, wait until gone, then add. Rejected: letting the component win
silently, which would remove something the tenant asked for elsewhere.

## 9. Cluster deletion

**Decided (no cascade):** `DeleteCluster` is refused while the cluster has
components: `FAILED_PRECONDITION`, naming them. **Decided (proposed):** the
CLI offers `cluster delete <id> --with-components`, which disables every
component (dependants first), waits for each, then deletes the cluster,
printing each step; the API stays two-step. A forced cluster deletion
(`DeleteClusterRequest.force`) forces its components too: Helm `Release`s are
taken out of the way by the existing `forceRelease`, and the platform's own
cloud resources for them (roles, queues) are still deleted.

## 10. Implementation steps

Each is mergeable alone. The release work in flight (release kinds, upgrade
mode, `pkg/release`) does not depend on these, and steps 1 to 3 here can run
in parallel with it.

| # | Delivers | Touches | Parallel with releases? |
|---|---|---|---|
| 1 | `Component` and `ComponentDefinition` kinds and CRDs; RBAC (tenants none, controller status, API server read/write components, read definitions) | `api/v1alpha1`, `config/crd`, `config/rbac`, `config/server` | Yes |
| 2 | A pure package `pkg/component`: values merge, locked-path check, schema validation, request checks of section 4.4 given plain inputs | `pkg/component` (new) | Yes |
| 3 | Contract: `component.proto`, `component_service.proto`; store methods for components (cloudstore) | `idl/cloud/v1`, `pkg/database/repository`, `pkg/database/cloudstore` | Yes |
| 4 | Write-only handlers and CLI; `DeleteCluster` and `UpdateCluster` reverse checks | `pkg/server/component` (new), `pkg/server/cluster`, `pkg/cli`, `cmd` | After 1-3 |
| 5 | Workload-cluster access for provider-helm: a `ProviderConfig` per cluster from the kubeconfig Secret | `pkg/crossplane` | Yes |
| 6 | First catalog component end to end: kyverno, then kyverno-policies (`requires`); chart credentials passed to provider-helm (section 3.2), with `get` on Secrets in the platform namespace | `pkg/crossplane/component` (new renderer), `pkg/reconcile/component`, `pkg/controller`, `config/rbac` | After 4-5 |
| 7 | Versions from releases: components read `Release` pins through `pkg/release` | `pkg/reconcile/component` | After release steps 3-5 |
| 8 | SQS and CloudWatch Events provider families (install, activation, cache) | `pkg/crossplane/aws` | Yes |
| 9 | The `Karpenter` renderer, the companion chart, and disable with the node wait | `pkg/crossplane/aws`, charts | After 6-8 |
| 10 | cert-manager with its add-on conflict; the AWS load balancer controller | definitions, renderer | After 6 |

### 10.1 Steps 1 and 3 as built

Where the code differs from the sketches above:

- Schemaless JSON (`values`, `effectiveValues`, `platformValues`, `valuesSchema`)
  is `*runtime.RawExtension`, as `contractStatus` is elsewhere in the API
  package, not `*apiextensionsv1.JSON`; the CRD is the same.
- The CRD cannot measure schemaless JSON, so the 64 KiB bound on values is
  enforced by the store (`ErrInvalid`) and will be by the API; the CRD
  checks that values are an object.
- `ComponentDefinition` has a status (`conditions`, with `Valid`, and
  `observedGeneration`) and the print column `Valid` reads it; the
  definition's name must be a DNS label and may not be in its own
  `requires`. `conflictsWithAddons` takes exactly the names
  `AWSAddon.name` accepts (a test keeps the two lists equal).
- A `Component` must be named `<cluster name>-<component>` (CEL), and is
  owned by its `Cluster` object (`pkg/owner`), labelled with the cluster,
  network and account names.
- The store claims the cluster when it stores a component (a new cluster
  etag, refused while the cluster is `DELETING`), as it claims a network
  for a new cluster, so a component create and a cluster delete cannot both
  win; `Clusters.Delete` refuses with `ErrInUse` naming the components.
- The contract's `ComponentStatus` has no inventory; the kind's status
  keeps it.
- The values model changed after the sketches: `defaults` became
  `platformValues` (locked), `protectedKeys` is gone (every platformValues
  leaf path is locked, computed keys later), and `userValues` (the values
  file, a YAML string) and the `GetComponentValues` RPC were added. The RPC
  takes only the definition's name, as the catalog is the same for every
  tenant. On the contract `protected_keys` (field 6) is reserved and the
  new fields take 8 and 9. In `pkg/component`: `CheckDefinition` (for the
  definition's `Valid` condition, set by the definition reconciler of step
  6: `CheckSchema`, the userValues file, its overlap with platformValues,
  `ValidateSchema`), and for the handlers of step 4 `PruneUnchanged`, then
  `CheckEnable` (check 10 is `LockedPathsTouched`), then `Merge`. One
  schema validator, the hand-written subset in `schema.go`. `ComponentService` is declared and not registered in the
  server until step 4.

## 11. Open questions

1. **Where the platform's own charts live** (Karpenter's `karpenter-defaults`).
   *Recommendation:* an OCI registry the operator controls, referenced in the
   definition; the charts are versioned and pinned through the release like
   any component.
2. **Who may write `ComponentDefinition`.** *Recommendation:* the
   `release-admin` role, as for releases; definitions change what every
   cluster runs.
3. **Values a tenant set that a new version no longer accepts.**
   *Recommendation:* the release is not applied to that cluster (it counts
   as failed for its stage) and the component shows the schema error; the
   operator either fixes the definition or the tenant fixes the values.
4. **Karpenter on a cluster whose subnets carry another cluster's discovery
   tag.** *Recommendation:* the default node class selects
   `kubernetes.io/cluster/<name>` in that case, as `TagSubnets` already
   reports; no new tagging.
5. **Existing installations on imported clusters.** *Recommendation:* out of
   scope; enabling warns that an existing installation is not detected
   (components.md, section 13).
