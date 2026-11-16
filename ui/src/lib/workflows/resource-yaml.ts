import { toJson, type JsonObject } from "@bufbuild/protobuf";
import { stringify } from "yaml";
import { CloudAccountSchema, type CloudAccount } from "../../gen/cloud/v1/tenant_service_pb";
import { NetworkSchema, type Network } from "../../gen/cloud/v1/network_pb";
import { ClusterSchema, type Cluster } from "../../gen/cloud/v1/cluster_pb";

export type YamlResource = { kind: "CloudAccount"; resource: CloudAccount } | { kind: "Network"; resource: Network } | { kind: "Cluster"; resource: Cluster };

/** CLI-compatible configuration envelope. Server-owned status and etags stay out. */
export function resourceYaml(input: YamlResource): string {
  let metadata: JsonObject;
  let spec: JsonObject;
  if (input.kind === "CloudAccount") {
    const account = input.resource;
    const json = toJson(CloudAccountSchema, account) as JsonObject;
    metadata = { name: account.accountName, id: account.id, tenant: account.tenantId };
    spec = {};
    const provider = account.provider.case;
    if (provider && json[provider] !== undefined) spec[provider] = json[provider];
  } else {
    const resource = input.resource;
    const json = (input.kind === "Network" ? toJson(NetworkSchema, input.resource) : toJson(ClusterSchema, input.resource)) as JsonObject;
    metadata = { name: resource.name, id: resource.id, tenant: resource.tenantId };
    if (resource.displayName) metadata.displayName = resource.displayName;
    if (Object.keys(resource.labels).length) metadata.labels = json.labels!;
    spec = json.spec as JsonObject ?? {};
  }
  return stringify({ apiVersion: "cluster-vending.io/v1", kind: input.kind, metadata, spec });
}
