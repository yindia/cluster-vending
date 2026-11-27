import assert from "node:assert/strict";
import test from "node:test";
import { create } from "@bufbuild/protobuf";
import { ComponentCloudResourcesSchema, ComponentIamAccess } from "../../gen/cloud/v1/component_cloud_pb";
import { componentIamSummary, formatPolicy } from "./component-cloud";

test("a component without an IAM role has no summary", () => {
  assert.equal(componentIamSummary(undefined), undefined);
  assert.equal(componentIamSummary(create(ComponentCloudResourcesSchema, {})), undefined);
});

test("the role lists its access, service accounts, managed policies and indented policy", () => {
  const summary = componentIamSummary(create(ComponentCloudResourcesSchema, { iamRole: {
    access: ComponentIamAccess.POD_IDENTITY, serviceAccounts: [{ namespace: "kube-system", name: "external-dns" }, { name: "solo" }],
    managedPolicyArns: ["arn:aws:iam::aws:policy/ReadOnlyAccess"], policy: '{"Version":"2012-10-17","Statement":[]}',
  } }));
  assert.deepEqual(summary, {
    access: "EKS Pod Identity", serviceAccounts: ["kube-system/external-dns", "default/solo"], managedPolicyArns: ["arn:aws:iam::aws:policy/ReadOnlyAccess"],
    policy: '{\n  "Version": "2012-10-17",\n  "Statement": []\n}',
  });
});

test("unset access is IRSA, the default; a policy that is not JSON is kept as it is", () => {
  const summary = componentIamSummary(create(ComponentCloudResourcesSchema, { iamRole: { policy: "${platform.policy}" } }));
  assert.equal(summary?.access, "IRSA (IAM roles for service accounts)");
  assert.equal(summary?.policy, "${platform.policy}");
  assert.equal(formatPolicy("  "), "");
});
