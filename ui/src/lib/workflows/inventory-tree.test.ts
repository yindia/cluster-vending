import assert from "node:assert/strict";
import test from "node:test";
import { create } from "@bufbuild/protobuf";
import { CloudAccountSchema } from "../../gen/cloud/v1/tenant_service_pb";
import { NetworkSchema } from "../../gen/cloud/v1/network_pb";
import { ClusterSchema } from "../../gen/cloud/v1/cluster_pb";
import { buildInventoryTree, filterInventoryTree } from "./inventory-tree";

const filters = { search: "", kind: "all", state: "all", provider: "all" };
const account = create(CloudAccountSchema, { id: "a", accountName: "Production", provider: { case: "aws", value: {} } });
const network = (id: string, parent = "a") => create(NetworkSchema, { id, name: "same-name", spec: { bootstrapId: parent } });
const cluster = (id: string, parent: string) => create(ClusterSchema, { id, name: "same-name", spec: { network: { networkId: parent } } });

test("inventory joins IDs, keeps same-name resources distinct, and retains missing parents", () => {
  const tree = buildInventoryTree([account], [network("n1"), network("n2"), network("orphan", "missing")], [cluster("c1", "n1"), cluster("c2", "n2"), cluster("c3", "orphan"), cluster("c4", "missing")]);
  assert.equal(tree[0].networks, 2);
  assert.equal(tree[0].clusters, 2);
  assert.equal(tree[0].children[0].children[0].resourceId, "c1");
  assert.equal(tree[0].children[1].children[0].resourceId, "c2");
  assert.equal(tree[1].name, "Unlinked resources");
  assert.equal(tree[1].clusters, 2);
  assert.equal(filterInventoryTree(tree, filters, new Set()).matches, 8);
});

test("filtered descendants reveal their ancestors despite collapse without counting context as matches", () => {
  const tree = buildInventoryTree([account], [network("n1"), network("n2")], [cluster("c1", "n1"), cluster("c2", "n2")]);
  const collapsed = new Set(["Account:a", "Network:n1"]);
  assert.equal(filterInventoryTree(tree, filters, collapsed).rows.length, 1);
  const result = filterInventoryTree(tree, { ...filters, search: "c1", kind: "Cluster" }, collapsed);
  assert.equal(result.matches, 1);
  assert.deepEqual(result.rows.map(row => [row.resourceId, row.depth, row.context]), [["a", 0, true], ["n1", 1, true], ["c1", 2, false]]);
  assert.equal(filterInventoryTree(tree, { ...filters, search: "absent" }, collapsed).rows.length, 0);
});
