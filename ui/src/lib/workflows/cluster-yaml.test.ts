import assert from "node:assert/strict";
import { test } from "node:test";
import { clone, create } from "@bufbuild/protobuf";
import { ClusterNetworkSchema } from "../../gen/cloud/v1/network_pb";
import { parse, stringify } from "yaml";
import { ClusterSchema, UpgradeMode } from "../../gen/cloud/v1/cluster_pb";
import { buildClusterUpdate, clusterDocument, clusterForm, emptyClusterForm, type ClusterForm } from "./clusters";
import { ClusterYamlError, clusterFormYaml, parseClusterFormYaml, type ClusterYamlContext } from "./cluster-yaml";

const networkId = "00000000-0000-4000-8000-000000000001";
const placement = { region: "us-west-2", networkId };
const regionOf = (id: string) => id === networkId ? "us-west-2" : undefined;
const createContext = { tenantId: "acme", regionOf };

const requested = create(ClusterSchema, {
  id: "cluster-1", tenantId: "acme", name: "orders", displayName: "Orders", etag: "etag-1", labels: { team: "orders" },
  spec: {
    region: "us-west-2", network: { networkId }, upgradeMode: UpgradeMode.MANUAL, releaseChannel: 3,
    controlPlane: { endpointAccess: { endpointPublicAccess: false, endpointPrivateAccess: true } },
    access: { adminPrincipals: ["arn:aws:iam::111122223333:role/admin"], oidc: { name: "corp", issuerUrl: "https://id.example.com", clientId: "k8s", requiredClaims: { aud: "k8s" } } },
    maintenanceWindow: { days: [1, 3], start: "02:00", duration: "4h" },
    provider: { case: "aws", value: { irsa: true, enabledClusterLogTypes: [1, 2], addons: [{ name: "metrics-server" }] } },
  },
});
const running = clone(ClusterSchema, requested);
running.spec!.network = create(ClusterNetworkSchema, { networkId, zoneIds: ["usw2-az1", "usw2-az2"], serviceCidr: "172.20.0.0/16" });
const editContext = { tenantId: "acme", baseline: requested };

function draft(): ClusterForm {
  return {
    ...emptyClusterForm, name: "payments", networkId, publicCIDRs: "203.0.113.0/24", displayName: "Payments",
    labels: '{"team":"payments"}', maintenanceDays: "6", maintenanceStart: "01:00", maintenanceDuration: "2h",
    oidcYaml: '{"name":"corp","issuerUrl":"https://id.example.com","clientId":"k8s"}',
  };
}

test("create YAML is the CLI manifest of the form and parses back to the same request", () => {
  const form = draft();
  const text = clusterFormYaml(form, placement);
  const manifest = parse(text);
  assert.equal(manifest.apiVersion, "cluster-vending.io/v1");
  assert.equal(manifest.kind, "Cluster");
  assert.deepEqual(manifest.metadata, { name: "payments", displayName: "Payments", labels: { team: "payments" } });
  assert.equal(manifest.spec.region, "us-west-2");
  assert.equal(manifest.spec.defaultNodePool, undefined, "the platform creates the system node group");
  assert.equal(manifest.spec.nodeGroups, undefined, "node groups are not part of the manifest");
  assert.equal(manifest.spec.tier, undefined);
  const parsed = parseClusterFormYaml(text, createContext);
  assert.deepEqual(clusterDocument(parsed, placement), clusterDocument(form, placement));
  assert.equal(parsed.networkId, networkId);
});

test("edit YAML round-trips with no changes, and one YAML edit masks exactly that field", () => {
  const baseline = clusterForm(requested, running);
  const text = clusterFormYaml(baseline, placement);
  assert.match(text, /usw2-az1/, "values the cluster was created with are shown");
  const unchanged = parseClusterFormYaml(text, editContext);
  assert.throws(() => buildClusterUpdate("acme", requested, unchanged, baseline), /No changes/);
  const manifest = parse(text);
  manifest.spec.maintenanceWindow.start = "03:30";
  const edited = parseClusterFormYaml(stringify(manifest), editContext);
  const request = buildClusterUpdate("acme", requested, edited, baseline);
  assert.deepEqual(request.updateMask?.paths, ["spec.maintenance_window"]);
  assert.equal(request.cluster?.spec?.maintenanceWindow?.start, "03:30");
  assert.equal(request.cluster?.labels && Object.keys(request.cluster.labels).length, 0);
  delete manifest.spec.access.oidc;
  manifest.metadata.labels = { team: "orders", cost: "42" };
  const cleared = buildClusterUpdate("acme", requested, parseClusterFormYaml(stringify(manifest), editContext), baseline);
  assert.deepEqual(cleared.updateMask?.paths, ["labels", "spec.access.oidc", "spec.maintenance_window"]);
  assert.equal(cleared.cluster?.spec?.access?.oidc, undefined);
});

function lineError(text: string, context: ClusterYamlContext = createContext): ClusterYamlError {
  try { parseClusterFormYaml(text, context); }
  catch (error) { assert.ok(error instanceof ClusterYamlError, String(error)); return error; }
  assert.fail("expected the YAML to be rejected");
}

test("YAML errors name the line they point at", () => {
  const base = clusterFormYaml(draft(), placement);
  const syntax = lineError("apiVersion: cluster-vending.io/v1\nkind: Cluster\nmetadata:\n  name: x\n spec: [\n");
  assert.match(syntax.message, /Invalid YAML/);
  const withUnknown = base.replace("spec:\n", "spec:\n  colour: blue\n");
  const unknown = lineError(withUnknown);
  assert.match(unknown.message, /colour/);
  assert.equal(unknown.line, withUnknown.split("\n").indexOf("  colour: blue") + 1);
  const badType = lineError(base.replace("start: 01:00", "start: 1am"));
  assert.equal(badType.line, base.split("\n").findIndex((line) => line.includes("start: 01:00")) + 1);
  assert.match(badType.message, /^Line \d+: spec\.maintenance_window\.start/);
  assert.match(base, /upgradeMode: UPGRADE_MODE_MANUAL/, "enums read as their names");
  const enumValue = lineError(base.replace(/upgradeMode: \S+/, "upgradeMode: WEEKLY"));
  assert.match(enumValue.message, /"WEEKLY" is not a cloud\.v1\.UpgradeMode value/);
  assert.equal(enumValue.line, base.split("\n").findIndex((line) => line.includes("upgradeMode")) + 1);
  assert.match(lineError(base.replace("kind: Cluster", "kind: Network")).message, /^Line 2: Use kind: Cluster/);
  assert.match(lineError(base.replace("spec:\n", "spec:\n  tier: TIER_1\n")).message, /inherited from its network/);
  assert.match(lineError(base.replace("name: payments", "name: payments\n  tenant: other")).message, /active tenant/);
  assert.match(lineError(base.replace("region: us-west-2", "region: eu-west-1")).message, /network's region/);
  assert.match(lineError("[]").message, /must be a YAML or JSON object/);
  assert.match(lineError(`metadata:\n  name: ${"a".repeat(70000)}\nspec: {}\n`).message, /64 KiB/);
  assert.match(lineError("a: &a [*a]\n").message, /YAML|alias/i);
});

test("YAML refuses node groups: the platform manages them", () => {
  const base = clusterFormYaml(draft(), placement);
  const user = lineError(base.replace("spec:\n", "spec:\n  nodeGroups:\n    - name: batch\n"));
  assert.match(user.message, /managed by the platform\. Remove spec\.nodeGroups/);
  assert.equal(user.line, base.split("\n").indexOf("spec:") + 2);
  assert.match(lineError(base.replace("spec:\n", "spec:\n  defaultNodePool:\n    instanceTypes: [m6i.large]\n")).message, /Remove spec\.defaultNodePool/);
  assert.match(lineError(base.replace("  aws: {}\n", "  aws:\n    defaultNodePool:\n      amiType: AWS_AMI_TYPE_BOTTLEROCKET_X86_64\n")).message, /Remove spec\.aws\.defaultNodePool/);
});

test("edit YAML cannot change the cluster's identity or placement", () => {
  const text = clusterFormYaml(clusterForm(requested, running), placement);
  assert.match(lineError(text.replace("name: orders", "name: renamed"), editContext).message, /metadata\.name cannot change/);
  assert.match(lineError(text.replace("region: us-west-2", "region: us-east-1"), editContext).message, /spec\.region cannot change/);
  assert.match(lineError(text.replace(networkId, "00000000-0000-4000-8000-000000000009"), editContext).message, /stays in the network/);
  const zones = buildClusterUpdate.bind(null, "acme", requested);
  const moved = parseClusterFormYaml(text.replace("usw2-az2", "usw2-az3"), editContext);
  assert.throws(() => zones(moved, clusterForm(requested, running)), /immutable/);
});

test("an unfinished create draft (no name or network yet) still converts both ways", () => {
  const text = clusterFormYaml({ ...emptyClusterForm }, { region: "", networkId: "" });
  const parsed = parseClusterFormYaml(text, createContext);
  assert.equal(parsed.name, "");
  assert.equal(parsed.networkId, "");
  const manifest = parse(text);
  manifest.spec.network = { networkId };
  const chosen = parseClusterFormYaml(stringify(manifest), createContext);
  assert.equal(chosen.networkId, networkId, "the region is taken from the chosen network");
});
