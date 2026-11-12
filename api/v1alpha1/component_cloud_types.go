package v1alpha1

// ComponentIAMAccess is how a component's pods get its IAM role.
// +kubebuilder:validation:Enum=IRSA;PodIdentity
type ComponentIAMAccess string

// IAM access modes of a component's role.
const (
	// ComponentIAMAccessIRSA is IAM roles for service accounts: the role
	// trusts the cluster's IAM OIDC provider, so the cluster needs
	// spec.aws.irsa. The chart's service account carries the role's ARN in
	// its eks.amazonaws.com/role-arn annotation; the platform team sets
	// that in the definition's values with the iam.roleArn placeholder
	// (each chart has its own key for it).
	ComponentIAMAccessIRSA ComponentIAMAccess = "IRSA"
	// ComponentIAMAccessPodIdentity is EKS Pod Identity: the platform
	// creates one pod identity association per service account.
	ComponentIAMAccessPodIdentity ComponentIAMAccess = "PodIdentity"
)

// ComponentServiceAccount is a service account of the component in the
// workload cluster.
type ComponentServiceAccount struct {
	// namespace of the service account.
	// +required
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`
	Namespace string `json:"namespace"`
	// name of the service account.
	// +required
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	Name string `json:"name"`
}

// ComponentIAMRole is the IAM role the platform creates for a component on
// each cluster it is enabled on. Its trust policy is always the
// platform's (the cluster's IAM OIDC provider for IRSA,
// pods.eks.amazonaws.com for PodIdentity), never written here.
// +kubebuilder:validation:XValidation:rule="(has(self.policy) && size(self.policy) > 0) || (has(self.managedPolicyArns) && size(self.managedPolicyArns) > 0)",message="iamRole needs a policy or managedPolicyArns"
// +kubebuilder:validation:XValidation:rule="(has(self.managedPolicyArns) ? size(self.managedPolicyArns) : 0) + (has(self.policy) && size(self.policy) > 0 ? 1 : 0) <= 10",message="an IAM role takes at most 10 policies: the policy and managedPolicyArns together"
type ComponentIAMRole struct {
	// access is how the component's pods get the role. Defaults to IRSA.
	// +optional
	// +kubebuilder:default=IRSA
	Access ComponentIAMAccess `json:"access,omitempty"`
	// serviceAccounts that may use the role.
	// +required
	// +listType=map
	// +listMapKey=namespace
	// +listMapKey=name
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=8
	ServiceAccounts []ComponentServiceAccount `json:"serviceAccounts"`
	// policy is an IAM policy document (JSON) the platform creates and
	// attaches to the role. It may use the placeholders of the definition
	// (account.id, cluster.name, ...), filled in per cluster; the iam.*
	// ones, which name this role and policy, are not available in it.
	// +optional
	// +kubebuilder:validation:MaxLength=6144
	Policy string `json:"policy,omitempty"`
	// managedPolicyArns are existing policies attached to the role: AWS
	// managed ones, or policies of the tenant account (account.id).
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=10
	// +kubebuilder:validation:items:Pattern=`^arn:aws(-cn|-us-gov)?:iam::(aws|[0-9]{12}|\$\{account\.id\}):policy/[A-Za-z0-9+=,.@/_-]{1,512}$`
	ManagedPolicyARNs []string `json:"managedPolicyArns,omitempty"`
}

// ComponentCloudResources is what the platform creates in the tenant's
// AWS account for a component, before its chart is installed, and removes
// after the chart is uninstalled.
type ComponentCloudResources struct {
	// iamRole for the component's service accounts.
	// +optional
	IAMRole *ComponentIAMRole `json:"iamRole,omitempty"`
}
