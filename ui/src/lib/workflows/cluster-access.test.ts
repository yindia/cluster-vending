import assert from "node:assert/strict";
import { test } from "node:test";
import { create } from "@bufbuild/protobuf";
import { ClusterSchema, ClusterState } from "../../gen/cloud/v1/cluster_pb";
import { ManagedResourceState } from "../../gen/cloud/v1/managed_resource_pb";
import { clusterAccessCommand, shellArgument } from "./cluster-access";

function cluster() {
  return create(ClusterSchema, {
    id: "cluster-id", tenantId: "acme", name: "logical-name", displayName: "Friendly label",
    spec: { region: "us-west-1", provider: { case: "aws", value: {} } },
    status: { provider: { case: "aws", value: { clusterArn: "arn:aws:eks:us-west-1:111122223333:cluster/actual-prefixed-name" } } },
  });
}
const resolve = (value = cluster()) => clusterAccessCommand(value, "acme", "cluster-id");

test("uses the physical EKS ARN name, never the platform name or display name", () => {
  assert.deepEqual(resolve(), {
    state: "ready", provider: "aws", region: "us-west-1", clusterName: "actual-prefixed-name", accountId: "111122223333",
    command: "aws eks update-kubeconfig --region 'us-west-1' --name 'actual-prefixed-name'",
  });
});

test("missing physical identity is pending instead of guessing the logical name", () => {
  const value = cluster();
  value.status!.provider = { case: undefined };
  const result = resolve(value);
  assert.equal(result.state, "pending");
  assert.ok(!("command" in result));
});

test("falls back only to the managed Cluster's external cloud name", () => {
  const value = cluster();
  value.status = create(ClusterSchema, { status: { managedResources: [
    { kind: "NodeGroup", externalName: "not-the-cluster" },
    { kind: "Cluster", name: "kubernetes-object-name", externalName: "EKS_actual-name", state: ManagedResourceState.READY },
  ] } }).status;
  const result = resolve(value);
  assert.equal(result.state, "ready");
  if (result.state === "ready") assert.equal(result.command, "aws eks update-kubeconfig --region 'us-west-1' --name 'EKS_actual-name'");
});

test("validates ARN external names in managed inventory", () => {
  const value = cluster();
  const arn = value.status!.provider.case === "aws" ? value.status!.provider.value.clusterArn : "";
  value.status!.provider = { case: undefined };
  value.status!.managedResources = create(ClusterSchema, { status: { managedResources: [{ kind: "Cluster", externalName: arn }] } }).status!.managedResources;
  const result = resolve(value);
  assert.equal(result.state, "ready");
  if (result.state === "ready") assert.equal(result.accountId, "111122223333");
});

test("rejects wrong ARN service, region, partition, account syntax and resource kind", () => {
  for (const arn of [
    "arn:aws:eks:us-east-1:111122223333:cluster/payments",
    "arn:aws-cn:eks:us-west-1:111122223333:cluster/payments",
    "arn:aws-fake:eks:us-west-1:111122223333:cluster/payments",
    "arn:aws:ec2:us-west-1:111122223333:cluster/payments",
    "arn:aws:eks:us-west-1:123:cluster/payments",
    "arn:aws:eks:us-west-1:111122223333:nodegroup/payments/workers/id",
    "arn:aws:eks:us-west-1:111122223333:cluster/payments\n",
    "arn:aws:eks:us-west-1:111122223333:cluster/$(whoami)",
  ]) {
    const value = cluster();
    if (value.status!.provider.case === "aws") value.status!.provider.value.clusterArn = arn;
    assert.equal(resolve(value).state, "invalid", arn);
  }
});

test("supports matching China and GovCloud partitions without inventing identity", () => {
  for (const [region, partition] of [["cn-north-1", "aws-cn"], ["us-gov-west-1", "aws-us-gov"]]) {
    const value = cluster();
    value.spec!.region = region;
    if (value.status!.provider.case === "aws") value.status!.provider.value.clusterArn = `arn:${partition}:eks:${region}:111122223333:cluster/payments`;
    assert.equal(resolve(value).state, "ready");
  }
});

test("invalid and missing regions never produce a command", () => {
  for (const region of ["", "us-west-1\n", "us-west-1;whoami"]) {
    const value = cluster(); value.spec!.region = region;
    const result = resolve(value);
    assert.equal(result.state, region ? "invalid" : "pending");
    assert.ok(!("command" in result));
  }
});

test("conflicting physical names or account IDs block the command", () => {
  for (const externalName of ["different-name", "arn:aws:eks:us-west-1:999999999999:cluster/actual-prefixed-name"]) {
    const value = cluster();
    value.status!.managedResources = create(ClusterSchema, { status: { managedResources: [{ kind: "Cluster", externalName }] } }).status!.managedResources;
    assert.equal(resolve(value).state, "invalid");
  }
});

test("missing, deleting and invalid inventory cannot be used as identity", () => {
  for (const state of [ManagedResourceState.MISSING, ManagedResourceState.DELETING]) {
    const value = cluster();
    value.status!.managedResources = create(ClusterSchema, { status: { managedResources: [{ kind: "Cluster", externalName: "actual-prefixed-name", state }] } }).status!.managedResources;
    assert.equal(resolve(value).state, "pending");
  }
  for (const externalName of ["name\n", "a'; touch /tmp/not-run", "name/extra"]) {
    const value = cluster(); value.status!.provider = { case: undefined };
    value.status!.managedResources = create(ClusterSchema, { status: { managedResources: [{ kind: "Cluster", externalName }] } }).status!.managedResources;
    assert.equal(resolve(value).state, "invalid");
  }
  const deleting = cluster(); deleting.status!.state = ClusterState.DELETING;
  assert.equal(resolve(deleting).state, "pending");
});

test("tenant and cluster identity mismatches block stale or cross-scope responses", () => {
  for (const value of [create(ClusterSchema, { ...cluster(), tenantId: "foreign" }), create(ClusterSchema, { ...cluster(), id: "other-id" })]) {
    assert.equal(resolve(value).state, "invalid");
  }
  assert.equal(clusterAccessCommand(cluster(), "", "cluster-id").state, "invalid");
});

test("unknown and unsupported providers never get an AWS command even with an ARN", () => {
  for (const provider of ["gcp", "azure", "unknown", undefined]) {
    const value = cluster();
    value.spec!.provider = { case: provider, value: {} } as never;
    const result = resolve(value);
    assert.equal(result.state, "unsupported");
    assert.ok(!("command" in result));
  }
});

test("POSIX quoting preserves apostrophes, substitutions, whitespace and separators as literal arguments", () => {
  assert.equal(shellArgument("a'b"), "'a'\\''b'");
  assert.equal(shellArgument("$(whoami); `id`\n spaced"), "'$(whoami); `id`\n spaced'");
  assert.equal(shellArgument(""), "''");
  assert.throws(() => shellArgument("name\0value"), /NUL/);
});


test("managed ARN accounts must agree even when a bare name is listed first", () => {
  const value = cluster(); value.status!.provider = { case: undefined };
  value.status!.managedResources = create(ClusterSchema, { status: { managedResources: [
    { kind: "Cluster", externalName: "same-name" },
    { kind: "Cluster", externalName: "arn:aws:eks:us-west-1:111122223333:cluster/same-name" },
    { kind: "Cluster", externalName: "arn:aws:eks:us-west-1:999999999999:cluster/same-name" },
  ] } }).status!.managedResources;
  assert.equal(resolve(value).state, "invalid");
});
