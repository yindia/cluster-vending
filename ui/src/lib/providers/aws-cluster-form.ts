import { toJson, type JsonObject, type JsonValue } from "@bufbuild/protobuf";
import { AwsClusterSpecSchema, AwsControlPlaneLogGroupSchema } from "../../gen/cloud/v1/aws_pb";
import type { Cluster } from "../../gen/cloud/v1/cluster_pb";
import type { ClusterForm } from "../workflows/clusters";
import { clusterList, formInteger, nestedFormYaml, optionalBoolean, typedFormJson } from "../workflows/cluster-form-values";

export const awsMutableClusterFields = {
  awsSupportType: "spec.aws.upgrade_policy.support_type",
  awsLogGroupEnabled: "spec.aws.control_plane_log_group", awsLogRetentionDays: "spec.aws.control_plane_log_group",
  awsLogTypes: "spec.aws.enabled_cluster_log_types",
  addonsYaml: "spec.aws.addons",
} as const;
export const awsCreateOnlyClusterFields = {
  awsAdditionalSecurityGroups: "spec.aws.additional_security_group_ids", awsAuthenticationMode: "spec.aws.access_config.authentication_mode",
} as const;
export type AwsClusterFormField = keyof typeof awsMutableClusterFields | keyof typeof awsCreateOnlyClusterFields;
export const emptyAwsClusterFields = { awsAdditionalSecurityGroups: "", awsAuthenticationMode: "", awsSupportType: "", awsLogGroupEnabled: "", awsLogRetentionDays: "", awsLogTypes: "", addonsYaml: "" };
/** Values set at creation (security groups, authentication mode) that the server
 * resolved are shown from `resolved` when the requested spec leaves them unset. */
export function awsClusterForm(cluster: Cluster, resolved?: Cluster) {
  const aws = cluster.spec?.provider.case === "aws" ? cluster.spec.provider.value : undefined;
  const actual = resolved?.spec?.provider.case === "aws" ? resolved.spec.provider.value : undefined;
  const json = aws ? toJson(AwsClusterSpecSchema, aws) as JsonObject : {};
  const authenticationMode = aws?.accessConfig?.authenticationMode || actual?.accessConfig?.authenticationMode || 0;
  const securityGroups = aws?.additionalSecurityGroupIds.length ? aws.additionalSecurityGroupIds : actual?.additionalSecurityGroupIds ?? [];
  return {
    awsAdditionalSecurityGroups: securityGroups.join(", "),
    awsAuthenticationMode: authenticationMode ? String(authenticationMode) : "",
    awsSupportType: aws?.upgradePolicy?.supportType ? String(aws.upgradePolicy.supportType) : "",
    awsLogGroupEnabled: aws?.controlPlaneLogGroup?.enabled === undefined ? "" : String(aws.controlPlaneLogGroup.enabled),
    awsLogRetentionDays: aws?.controlPlaneLogGroup?.retentionDays ? String(aws.controlPlaneLogGroup.retentionDays) : "",
    awsLogTypes: aws?.enabledClusterLogTypes.join(", ") ?? "",
    addonsYaml: aws?.addons.length ? JSON.stringify(json.addons, null, 2) : "",
  };
}
export function awsClusterFormValue(field: AwsClusterFormField, form: ClusterForm): JsonValue | undefined {
  switch (field) {
    case "awsAuthenticationMode": return form.awsAuthenticationMode ? formInteger(form.awsAuthenticationMode, 0, 3, "Authentication mode") : 0;
    case "awsSupportType": return form.awsSupportType ? formInteger(form.awsSupportType, 0, 2, "Support type") : 0;
    case "awsAdditionalSecurityGroups": {
      const values = clusterList(form.awsAdditionalSecurityGroups);
      if (values.length > 4 || new Set(values).size !== values.length || values.some(id => !/^sg-[0-9a-f]{8,17}$/.test(id))) throw new Error("Enter up to four unique additional security group IDs.");
      return values;
    }
    case "awsLogTypes": {
      const aliases: Record<string, number> = { api: 1, audit: 2, authenticator: 3, controllerManager: 4, scheduler: 5 };
      const values = clusterList(form.awsLogTypes).map(value => aliases[value] ?? formInteger(value, 1, 5, "Log type"));
      if (values.length > 5 || new Set(values).size !== values.length) throw new Error("Log types must be unique.");
      return values;
    }
    case "awsLogGroupEnabled": case "awsLogRetentionDays": {
      if (!form.awsLogGroupEnabled && !form.awsLogRetentionDays) return undefined;
      const enabled = optionalBoolean(form.awsLogGroupEnabled, "Log group enabled");
      return typedFormJson(AwsControlPlaneLogGroupSchema, { ...(enabled !== undefined ? { enabled } : {}), ...(form.awsLogRetentionDays ? { retentionDays: formInteger(form.awsLogRetentionDays, 1, 3653, "Log retention") } : {}) });
    }
    case "addonsYaml": {
      const value = nestedFormYaml(form.addonsYaml) ?? [];
      const json = typedFormJson(AwsClusterSpecSchema, { addons: value }) as JsonObject;
      return json.addons ?? [];
    }
  }
}
