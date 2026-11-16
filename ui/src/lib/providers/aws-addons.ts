import { getOption } from "@bufbuild/protobuf";
import { field } from "@/gen/buf/validate/validate_pb";
import { AwsAddonSchema, type AwsAddon, type AwsAddonStatus } from "@/gen/cloud/v1/aws_pb";
import { ManagedResourceState } from "@/gen/cloud/v1/managed_resource_pb";
import type { InstallationStatus } from "@/lib/workflows/installation-status";

const rules = getOption(AwsAddonSchema.field.name, field);
// Discover optional names from the generated contract, rather than maintaining
// a second add-on catalog. These five defaults mirror the platform's EKS plan.
const supportedNames = rules.type.case === "string" ? rules.type.value.in : [];
const defaults = new Set(["vpc-cni", "coredns", "kube-proxy", "eks-pod-identity-agent", "aws-ebs-csi-driver"]);

export function awsAddonInventory(desired: readonly AwsAddon[] | undefined, observed: readonly AwsAddonStatus[] | undefined) {
  const names = [...new Set([...supportedNames, ...(desired?.map((addon) => addon.name) ?? []), ...(observed?.map((addon) => addon.name) ?? [])])];
  return names.map((name) => {
    const target = desired?.find((addon) => addon.name === name);
    const running = observed?.find((addon) => addon.name === name);
    const selected = defaults.has(name) || !!target;
    let status: InstallationStatus;
    if (running?.state === ManagedResourceState.DELETING) status = { label: "Deleting", tone: "warning", key: "deleting" };
    else if (running?.state === ManagedResourceState.FAILED) status = { label: "Failed", tone: "error", key: "failed" };
    else if (!selected && desired !== undefined) status = running
      ? { label: "Disabling", tone: "warning", key: "disabling" }
      : { label: "Disabled", tone: "neutral", key: "disabled" };
    else if (!running) status = selected ? { label: "Pending", tone: "warning", key: "pending" } : { label: "Not reported", tone: "neutral", key: "not-reported" };
    else status = { label: ManagedResourceState[running.state]?.toLowerCase().replaceAll("_", " ").replace(/^./, (letter) => letter.toUpperCase()) ?? "Not reported",
      tone: running.state === ManagedResourceState.READY ? "success" : "warning", key: String(running.state) };
    return { name, target: target?.version || (status.key === "disabled" ? "—" : "Selected by AWS"), running, status,
      message: status.key === "disabled" ? "Not enabled for this cluster." : status.key === "disabling" ? "Not selected; an installed add-on is still reported."
        : running?.message || (running ? "No message reported" : selected ? "Waiting for observation" : "Selection has not been reported.") };
  });
}
