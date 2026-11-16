import assert from "node:assert/strict";
import test from "node:test";
import { create } from "@bufbuild/protobuf";
import { AwsAddonSchema, AwsAddonStatusSchema } from "@/gen/cloud/v1/aws_pb";
import { PlatformComponentSchema } from "@/gen/cloud/v1/cluster_pb";
import { ComponentManagement, ComponentPhase, ComponentSchema } from "@/gen/cloud/v1/component_pb";
import { ManagedResourceState } from "@/gen/cloud/v1/managed_resource_pb";
import { awsAddonInventory } from "@/lib/providers/aws-addons";
import { componentInstallationStatus, platformInstallationStatus } from "./installation-status";

test("disabled tenant catalog entries never inherit a stale Ready phase", () => {
  const item = create(ComponentSchema, { management: ComponentManagement.TENANT, status: { phase: ComponentPhase.READY } });
  assert.deepEqual(componentInstallationStatus(item), { label: "Disabled", tone: "neutral", key: "disabled" });
  item.id = "enabled"; item.generation = 2n; item.status!.observedGeneration = 1n;
  assert.equal(componentInstallationStatus(item).label, "Reconciling");
  item.status!.observedGeneration = 2n;
  assert.equal(componentInstallationStatus(item).label, "Ready");
  item.status!.phase = ComponentPhase.DELETING;
  assert.equal(componentInstallationStatus(item).label, "Deleting");
});

test("platform components need no tenant ID and removal stays visible", () => {
  const item = create(ComponentSchema, { management: ComponentManagement.PLATFORM, status: { phase: ComponentPhase.READY } });
  assert.equal(componentInstallationStatus(item).label, "Ready");
  const platform = create(PlatformComponentSchema, { name: "capsule", phase: ComponentPhase.PENDING });
  assert.equal(platformInstallationStatus(platform, false).label, "Disabled");
  platform.phase = ComponentPhase.READY;
  assert.equal(platformInstallationStatus(platform, false).label, "Disabling");
  platform.phase = ComponentPhase.FAILED;
  assert.equal(platformInstallationStatus(platform, false).label, "Failed");
});

test("optional EKS add-ons are Disabled only when selection and observations confirm absence", () => {
  const rows = awsAddonInventory([], []);
  assert.equal(rows.find((row) => row.name === "aws-efs-csi-driver")?.status.label, "Disabled");
  assert.equal(rows.find((row) => row.name === "metrics-server")?.status.tone, "neutral");
  assert.equal(rows.find((row) => row.name === "vpc-cni")?.status.label, "Pending");
  const unknown = awsAddonInventory(undefined, []);
  assert.equal(unknown.find((row) => row.name === "aws-efs-csi-driver")?.status.label, "Not reported");
  const desired = [create(AwsAddonSchema, { name: "metrics-server" })];
  assert.equal(awsAddonInventory(desired, []).find((row) => row.name === "metrics-server")?.status.label, "Pending");
});

test("selected, removing and failed EKS add-ons retain distinct observed states", () => {
  const desired = [create(AwsAddonSchema, { name: "metrics-server" })];
  const running = create(AwsAddonStatusSchema, { name: "metrics-server", state: ManagedResourceState.READY, version: "v1" });
  const status = (selected = desired) => awsAddonInventory(selected, [running]).find((row) => row.name === running.name)!.status.label;
  assert.equal(status(), "Ready");
  assert.equal(status([]), "Disabling");
  running.state = ManagedResourceState.DELETING;
  assert.equal(status([]), "Deleting");
  running.state = ManagedResourceState.FAILED;
  assert.equal(status([]), "Failed");
});
