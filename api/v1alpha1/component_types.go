package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ComponentFinalizer holds a Component until what it installed is removed.
const ComponentFinalizer = "cloud.cluster-vending.io/component"

// MaxComponentValuesBytes bounds a component's values, serialized as JSON.
// The CRD cannot measure schemaless JSON, so the store and the API check it.
const MaxComponentValuesBytes = 64 << 10

// ComponentSpec is what a tenant asks for: one component on one cluster.
// +kubebuilder:validation:XValidation:rule="self.clusterID == oldSelf.clusterID",message="clusterID is immutable"
// +kubebuilder:validation:XValidation:rule="self.component == oldSelf.component",message="component is immutable"
type ComponentSpec struct {
	// clusterID is the cluster the component is installed into: the API's
	// Cluster.id. The object is owned by that Cluster.
	// +required
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`
	ClusterID string `json:"clusterID"`
	// component is the ComponentDefinition name, e.g. karpenter.
	// +required
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`
	Component string `json:"component"`
	// version pins a version on a manual cluster. Unset: the version the
	// release that applies to the cluster gives its Kubernetes minor.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	Version string `json:"version,omitempty"`
	// values are the tenant's overrides only, a JSON object of at most
	// 64 KiB: what differs from the definition's userValues file. The
	// definition's platformValues and computed values are never copied
	// here, and may not be set. Not for secrets.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:validation:Type=object
	Values *runtime.RawExtension `json:"values,omitempty"`
}

// ComponentStatus is written by the controller only.
type ComponentStatus struct {
	// phase: Pending, Installing, Ready, Degraded, Failed, Deleting.
	// +optional
	// +kubebuilder:validation:Enum=Pending;Installing;Ready;Degraded;Failed;Deleting
	Phase string `json:"phase,omitempty"`
	// Conditions: Valid (the request's checks hold now), Synced (the
	// platform's objects are applied), Ready (installed and healthy as far
	// as the install mechanism reports).
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// observedGeneration is the spec generation the status describes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// cluster is the cluster's name, resolved from spec.clusterID, for
	// people.
	// +optional
	Cluster string `json:"cluster,omitempty"`
	// installedVersion is what runs.
	// +optional
	InstalledVersion string `json:"installedVersion,omitempty"`
	// targetVersion is what it is moving to.
	// +optional
	TargetVersion string `json:"targetVersion,omitempty"`
	// release the version came from (empty for a pinned version).
	// +optional
	Release string `json:"release,omitempty"`
	// effectiveValues are the merged values that were applied, with
	// platform-computed keys included and no secrets.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:validation:Type=object
	EffectiveValues *runtime.RawExtension `json:"effectiveValues,omitempty"`
	// inventory lists the objects the platform has for this component, in
	// creation order.
	// +optional
	// +listType=atomic
	Inventory []InventoryEntry `json:"inventory,omitempty"`
	// message explains the phase when it is not Ready.
	// +optional
	Message string `json:"message,omitempty"`
	// since is when the component entered its phase.
	// +optional
	Since *metav1.Time `json:"since,omitempty"`
	// chart is the Helm release the phase is about, while the component
	// is not Ready.
	// +optional
	Chart string `json:"chart,omitempty"`
	// helmState is Helm's state of that release: deployed, failed,
	// uninstalling, pending-install, ...
	// +optional
	HelmState string `json:"helmState,omitempty"`
	// lastError is the install mechanism's last error for that release.
	// +optional
	LastError string `json:"lastError,omitempty"`
	// failures are its failed attempts in a row.
	// +optional
	Failures int32 `json:"failures,omitempty"`
	// recoveries is how many times a stuck Helm release of the component
	// was recovered at this version.
	// +optional
	Recoveries int32 `json:"recoveries,omitempty"`
}

// Component is one catalog component enabled on one cluster of a tenant.
// It lives in the tenant namespace and is named <cluster name>-<component>.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=cvcomp
// +kubebuilder:subresource:status
// +kubebuilder:validation:XValidation:rule="self.metadata.name.endsWith('-' + self.spec.component)",message="a component is named <cluster name>-<component>"
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.status.cluster`
// +kubebuilder:printcolumn:name="Component",type=string,JSONPath=`.spec.component`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.status.installedVersion`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Component struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec   ComponentSpec   `json:"spec"`
	Status ComponentStatus `json:"status,omitzero"`
}

// ComponentList is a list of Component.
// +kubebuilder:object:root=true
type ComponentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Component `json:"items"`
}

// HelmCredentialsType is the kind of Secret that opens a private chart
// repository.
// +kubebuilder:validation:Enum=BasicAuth;DockerConfigJSON
type HelmCredentialsType string

// Credential types.
const (
	// HelmCredentialsBasicAuth: a kubernetes.io/basic-auth Secret (keys
	// username and password), for https Helm repositories and OCI
	// registries that take basic auth.
	HelmCredentialsBasicAuth HelmCredentialsType = "BasicAuth"
	// HelmCredentialsDockerConfigJSON: a kubernetes.io/dockerconfigjson
	// Secret, for OCI registries such as ECR whose pull secret is
	// refreshed outside the platform.
	HelmCredentialsDockerConfigJSON HelmCredentialsType = "DockerConfigJSON" //nolint:gosec // a Secret type, not a credential
)

// SecretReference names a Secret in the management cluster.
type SecretReference struct {
	// namespace of the Secret: the platform's own namespace, where the
	// operator creates it. Never a tenant namespace.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`
	Namespace string `json:"namespace"`
	// name of the Secret.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	Name string `json:"name"`
}

// HelmCredentials open a private chart repository. They are a reference
// to a Secret the operator creates, never the secret itself: a definition
// has no field for a username, password or token.
//
// The controller (a later step) passes them to provider-helm v1.5.0
// (helm.m.crossplane.io/v1beta1 Release), whose only credential field is
// spec.forProvider.chart.pullSecretRef: a LocalSecretReference, a Secret
// with keys username and password in the Release's own (tenant)
// namespace. It copies a BasicAuth Secret there as it is; of a
// DockerConfigJSON Secret it writes the username and password for the
// repository's registry host (.dockerconfigjson auths) into such a
// Secret. Without credentials provider-helm uses its default credential
// chain.
type HelmCredentials struct {
	// type of the Secret.
	// +required
	Type HelmCredentialsType `json:"type"`
	// secretRef is the Secret holding them.
	// +required
	SecretRef SecretReference `json:"secretRef"`
}

// HelmChartSource is where a chart comes from. Its version is not here: it
// comes from the Release that applies to the cluster, per Kubernetes minor
// (Release.spec.content.kubernetes[].components), or from a version a
// tenant pinned on a manual cluster.
// +kubebuilder:validation:XValidation:rule="!has(self.plainHTTP) || !self.plainHTTP || self.repository.startsWith('oci://')",message="plainHTTP is for oci:// registries"
type HelmChartSource struct {
	// repository is an https:// Helm repository URL or an oci:// registry
	// path (provider-helm forProvider.chart.repository).
	// +required
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^(https|oci)://[^ ]+$`
	Repository string `json:"repository"`
	// chart name in the repository (provider-helm forProvider.chart.name).
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9_.]*[a-z0-9])?$`
	Chart string `json:"chart"`
	// credentials for a private repository. Unset: a public one.
	// +optional
	Credentials *HelmCredentials `json:"credentials,omitempty"`
	// insecureSkipTLSVerify skips the check of the repository's TLS
	// certificate (provider-helm forProvider.insecureSkipTLSVerify). For
	// test registries only.
	// +optional
	InsecureSkipTLSVerify bool `json:"insecureSkipTLSVerify,omitempty"`
	// plainHTTP fetches the chart over HTTP (provider-helm
	// forProvider.plainHTTP), oci:// only. For test registries only.
	// +optional
	PlainHTTP bool `json:"plainHTTP,omitempty"`
}

// HelmCompanion is a chart the platform owns, installed after the main
// chart is Ready.
type HelmCompanion struct {
	// name of the companion's Helm release.
	// +required
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]{0,51}[a-z0-9])?$`
	Name string `json:"name"`

	HelmChartSource `json:",inline"`
}

// HelmInstall is a chart, an optional chart of its CustomResourceDefinitions
// installed before it, and optional companion charts installed after it.
type HelmInstall struct {
	HelmChartSource `json:",inline"`

	// crds is a chart that holds only the component's
	// CustomResourceDefinitions, as templates (so Helm upgrades them; Helm
	// installs a chart's crds/ directory once and never upgrades it). It is
	// installed, and upgraded, before the main chart, at the same version
	// the release pins for the component; the main chart then runs with
	// skipCRDs (provider-helm Release forProvider.skipCRDs). It gets no
	// values. Unset: the main chart's own CRDs, if any, which are never
	// upgraded.
	// +optional
	CRDs *HelmChartSource `json:"crds,omitempty"`

	// companions are charts the platform owns, installed in order after the
	// main chart is Ready (e.g. Karpenter's default node class).
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=8
	Companions []HelmCompanion `json:"companions,omitempty"`
}

// ComponentInstall is how a component reaches a workload cluster: exactly
// one mechanism. Components are never EKS add-ons.
// +kubebuilder:validation:XValidation:rule="has(self.helm)",message="helm is the only install mechanism so far"
type ComponentInstall struct {
	// helm installs the component with provider-helm.
	// +optional
	Helm *HelmInstall `json:"helm,omitempty"`
}

// CloudProvider is a cloud a component can be installed on: aws, gcp or
// azure, the names of the cluster's provider.
// +kubebuilder:validation:Enum=aws;gcp;azure
type CloudProvider string

// Cloud providers.
const (
	CloudProviderAWS   CloudProvider = "aws"
	CloudProviderGCP   CloudProvider = "gcp"
	CloudProviderAzure CloudProvider = "azure"
)

// ComponentManagement says who controls a component.
// +kubebuilder:validation:Enum=Platform;Tenant
type ComponentManagement string

// Who controls a component.
const (
	// ComponentManagementPlatform: installed with every cluster.
	ComponentManagementPlatform ComponentManagement = "Platform"
	// ComponentManagementTenant: enabled by a tenant on a cluster.
	ComponentManagementTenant ComponentManagement = "Tenant"
)

// ComponentDefinitionSpec is one entry of the catalog.
// +kubebuilder:validation:XValidation:rule="has(self.install) != has(self.includes)",message="exactly one of install and includes is set"
// +kubebuilder:validation:XValidation:rule="!has(self.install) || has(self.__namespace__)",message="namespace is required with install"
// +kubebuilder:validation:XValidation:rule="!has(self.includes) || !has(self.internal) || !self.internal",message="a bundle (includes) is not internal: bundles do not nest"
type ComponentDefinitionSpec struct {
	// description shown to tenants.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	Description string `json:"description"`
	// disabled switches the component off without deleting its definition:
	// a Platform component is not installed (clusters that have it remove
	// it), and a Tenant one cannot be enabled. What requires it, and the
	// members of a disabled bundle, are off with it. The platform team's
	// to set: a new build keeps it when it updates a shipped definition.
	// +optional
	Disabled bool `json:"disabled,omitempty"`
	// management says who controls the component. Platform: installed
	// with every cluster by the cluster's own reconcile, once the cluster
	// can be reached, with fixed values; no Component object exists for
	// it and the Component API refuses its name. Tenant (the default): a
	// tenant enables and disables it on a cluster (a Component).
	// +optional
	// +kubebuilder:default=Tenant
	Management ComponentManagement `json:"management,omitempty"`
	// provider is the cloud the component is installed on: a cluster of
	// another provider never gets it, and a tenant cannot enable it there.
	// Every definition is for one cloud; the same component for another
	// cloud is another definition, named <name>-<provider> (kyverno-gcp).
	// Immutable.
	// +required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="provider is immutable; a definition for another cloud is another definition"
	Provider CloudProvider `json:"provider"`
	// internal marks a definition that is only installed as a member of a
	// bundle (includes): tenants cannot enable it by itself, and the
	// catalog they see does not list it.
	// +optional
	Internal bool `json:"internal,omitempty"`
	// install is how the component reaches a workload cluster. Opaque to
	// tenants: the API never returns it. Exactly one of install and
	// includes is set.
	// +optional
	Install *ComponentInstall `json:"install,omitempty"`
	// includes makes the definition a bundle: one component made of
	// several charts, each a definition of its own (internal: true) with
	// its chart, namespace, version pin and requires. Enabling the bundle
	// installs every member in requires order, each once the one before
	// is Ready; disabling removes them in reverse; the bundle is Ready
	// when every member is. A release that offers a bundle pins every
	// member. A bundle is not a member of another bundle.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`
	Includes []string `json:"includes,omitempty"`
	// namespace in the workload cluster. Required with install; a bundle
	// has none (its members have theirs).
	// +optional
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`
	Namespace string `json:"namespace,omitempty"`
	// requires lists other components that must be Ready first.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`
	Requires []string `json:"requires,omitempty"`
	// versionGroup names components that are versions of one product (the
	// Istio charts, Kyverno and its policies): a release must pin every
	// member it pins, on each Kubernetes minor, at the same version. Unset:
	// none.
	// +optional
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`
	VersionGroup string `json:"versionGroup,omitempty"`
	// upgradeOneMinorAtATime: the component supports in-place upgrades of
	// one minor version at a time only (Istio). A channel does not move to
	// a release whose pin skips a minor of the component relative to the
	// release the channel offers now.
	// +optional
	UpgradeOneMinorAtATime bool `json:"upgradeOneMinorAtATime,omitempty"`
	// conflictsWithAddons lists EKS add-ons the cluster must not have in
	// spec.aws.addons while this component is enabled. The names are the
	// add-ons AWSAddon.name accepts.
	// +optional
	// +listType=set
	// +kubebuilder:validation:items:Enum=vpc-cni;coredns;kube-proxy;eks-pod-identity-agent;aws-ebs-csi-driver;aws-efs-csi-driver;aws-fsx-csi-driver;aws-mountpoint-s3-csi-driver;snapshot-controller;cert-manager;metrics-server;kube-state-metrics
	ConflictsWithAddons []string `json:"conflictsWithAddons,omitempty"`
	// cloudResources is what the platform creates in the tenant's account
	// for the component before installing it: an IAM role so far. Unset:
	// none.
	// +optional
	CloudResources *ComponentCloudResources `json:"cloudResources,omitempty"`
	// platformValues are set by the platform team for every tenant, and
	// locked: every leaf path in them wins over the tenant's values, and
	// an enable or update whose overrides touch one is refused, naming the
	// path. Tenants never see them in the values file. Strings may use the
	// placeholders (${cluster.name}, ${iam.roleArn}, ...), filled in per
	// cluster by the controller.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:validation:Type=object
	PlatformValues *runtime.RawExtension `json:"platformValues,omitempty"`
	// userValues is the starting values file tenants pull (cluster-vending
	// component values <name>), edit and send with enable/update; only keys
	// a tenant may change. YAML kept as written, so the platform team's
	// comments survive as they do in a chart's values.yaml. The definition
	// is Valid only when it parses into an object, shares no path with
	// platformValues (neither the same leaf nor one inside the other) and
	// validates against valuesSchema. A tenant's keys equal to the file's
	// are not stored (pkg/component PruneUnchanged), so a later change to
	// the file reaches tenants who did not edit that key.
	// +optional
	// +kubebuilder:validation:MaxLength=65536
	UserValues string `json:"userValues,omitempty"`
	// valuesSchema is a JSON Schema, written by the platform team, of what
	// a tenant may override; userValues must validate against it too.
	// Unset: no overrides are accepted (and userValues must be empty).
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:validation:Type=object
	ValuesSchema *runtime.RawExtension `json:"valuesSchema,omitempty"`
}

// ComponentDefinitionStatus is written by the controller.
type ComponentDefinitionStatus struct {
	// Conditions: Valid.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// observedGeneration is the spec generation the status describes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// ComponentDefinition is an operator-owned catalog entry: what a tenant
// may enable and how the platform installs it. Written by release-admin.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=cvcompdef
// +kubebuilder:subresource:status
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$')",message="a component definition is named as a DNS label"
// +kubebuilder:validation:XValidation:rule="!has(self.spec.requires) || !(self.metadata.name in self.spec.requires)",message="a component cannot require itself"
// +kubebuilder:printcolumn:name="Requires",type=string,JSONPath=`.spec.requires`
// +kubebuilder:printcolumn:name="IAM",type=string,JSONPath=`.spec.cloudResources.iamRole.access`
// +kubebuilder:printcolumn:name="Disabled",type=boolean,JSONPath=`.spec.disabled`
// +kubebuilder:printcolumn:name="Valid",type=string,JSONPath=`.status.conditions[?(@.type=="Valid")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ComponentDefinition struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec   ComponentDefinitionSpec   `json:"spec"`
	Status ComponentDefinitionStatus `json:"status,omitzero"`
}

// ComponentDefinitionList is a list of ComponentDefinition.
// +kubebuilder:object:root=true
type ComponentDefinitionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ComponentDefinition `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion,
			&Component{}, &ComponentList{},
			&ComponentDefinition{}, &ComponentDefinitionList{})
		return nil
	})
}
