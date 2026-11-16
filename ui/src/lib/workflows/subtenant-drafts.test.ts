import assert from "node:assert/strict";
import test from "node:test";
import { emptySubTenantDraft, identityReferences, namespaceKeys, quotaEntries, validateSubTenantDraft } from "./subtenant-drafts";

test("namespace keys enforce uniqueness, DNS limits and append-only edits", () => {
  assert.deepEqual(namespaceKeys("team-a, team-b"), ["team-a", "team-b"]);
  assert.deepEqual(namespaceKeys("a".repeat(27)), ["a".repeat(27)]);
  for (const value of ["", "Team", "a.b", "-team", "team-", "a".repeat(28), "team team"]) assert.throws(() => namespaceKeys(value));
  const draft = { ...emptySubTenantDraft, namespaceKeys: "original added", namespaceCount: "2", quotas: "pods=10" };
  assert.doesNotThrow(() => validateSubTenantDraft(draft, undefined, ["original"]));
  assert.throws(() => validateSubTenantDraft({ ...draft, namespaceKeys: "added" }, undefined, ["original"]), /retained/);
});

test("quota entries remain strings and reject ambiguity or prototype keys", () => {
  assert.deepEqual(quotaEntries("requests.cpu=2\nrequests.memory=4Gi\npods=10"), { "requests.cpu": "2", "requests.memory": "4Gi", pods: "10" });
  for (const value of ["", "pods", "pods=", "pods=10\npods=20", "__proto__=bad", "pods=10 20", "pods=1=2", "pods=1.5", "pods=-1", "unknown=1"]) assert.throws(() => quotaEntries(value));
});

test("quota and access validation does not grant roles or reduce requested namespace capacity", () => {
  const draft = { ...emptySubTenantDraft, namespaceKeys: "team-a team-b", namespaceCount: "2", identityRefs: "identity-a,identity-b", quotas: "pods=10" };
  assert.doesNotThrow(() => validateSubTenantDraft(draft));
  for (const quota of ["0", "1", "65", "1.5", "-1"]) assert.throws(() => validateSubTenantDraft({ ...draft, namespaceCount: quota }));
  assert.deepEqual(identityReferences(draft.identityRefs), ["identity-a", "identity-b"]);
  assert.throws(() => identityReferences("same same"), /duplicates/);
});
