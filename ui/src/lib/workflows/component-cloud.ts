import { ComponentIamAccess, type ComponentCloudResources } from "../../gen/cloud/v1/component_cloud_pb";

/** The IAM role the platform creates for a component, as the detail page lists it. */
export interface ComponentIamSummary {
  access: string;
  serviceAccounts: string[];
  managedPolicyArns: string[];
  /** The policy document, indented when it is JSON; empty when the role has none. */
  policy: string;
}

const accessLabels: Record<ComponentIamAccess, string> = {
  [ComponentIamAccess.UNSPECIFIED]: "IRSA (IAM roles for service accounts)",
  [ComponentIamAccess.IRSA]: "IRSA (IAM roles for service accounts)",
  [ComponentIamAccess.POD_IDENTITY]: "EKS Pod Identity",
};

/** The policy as JSON indented by two spaces; text that is not JSON is shown as it is. */
export function formatPolicy(policy: string): string {
  if (!policy.trim()) return "";
  try { return JSON.stringify(JSON.parse(policy), null, 2); } catch { return policy; }
}

/** Undefined when the component creates no IAM role. Unset access is IRSA, the server's default. */
export function componentIamSummary(resources: ComponentCloudResources | undefined): ComponentIamSummary | undefined {
  const role = resources?.iamRole;
  if (!role) return undefined;
  return {
    access: accessLabels[role.access] ?? `Unknown (${role.access})`,
    serviceAccounts: role.serviceAccounts.map((account) => `${account.namespace || "default"}/${account.name}`),
    managedPolicyArns: [...role.managedPolicyArns],
    policy: formatPolicy(role.policy),
  };
}
