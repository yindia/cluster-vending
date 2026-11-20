import assert from "node:assert/strict";
import { test } from "node:test";
import { create } from "@bufbuild/protobuf";
import { ClusterPolicySnapshotSummarySchema, ClusterViolationSchema, ClusterViolationEventSchema } from "../../gen/cloud/v1/violation_pb";
import { collectPolicySnapshot, emptyPolicyFilters, filterPolicyRows, policyFilterOptions, policyFilterValue, staleSnapshotLabel } from "./cluster-violations";

const summary = () => create(ClusterPolicySnapshotSummarySchema, { collectedAt: { seconds: 100n }, notices: ["Limited source coverage"] });
const violation = (id: string, kind = "Pod") => create(ClusterViolationSchema, { id, engine: "kyverno", policy: { kind: "Policy", namespace: "tenant-a", name: "require-label" }, resource: { kind, namespace: "default", name: id }, result: "fail", enforcement: "unknown", message: "Missing required label" });

test("collects every retained page before search/filter and preserves coverage", async () => {
  const tokens: string[] = [];
  const result = await collectPolicySnapshot(async token => {
    tokens.push(token);
    return { items: [violation(token || "first", token ? "Deployment" : "Pod")], summary: summary(), nextPageToken: token ? "" : "second" };
  });
  assert.deepEqual(tokens, ["", "second"]);
  assert.equal(filterPolicyRows(result.rows, { ...emptyPolicyFilters, kind: "value:Deployment", search: "SECOND missing" })[0]?.id, "second");
  assert.deepEqual(result.summary?.notices, ["Limited source coverage"]);
});

test("rejects changed collection timestamps and coverage instead of mixing pages", async () => {
  for (const change of ["timestamp", "coverage", "retention"]) {
    await assert.rejects(collectPolicySnapshot(async token => {
      const value = summary();
      if (token && change === "timestamp") value.collectedAt!.seconds++;
      if (token && change === "coverage") value.incomplete = true;
      return { items: [violation("row")], summary: value, retentionNotice: token && change === "retention" ? "changed" : "retained", nextPageToken: token ? "" : "next" };
    }), /snapshot changed/);
  }
});

test("later-page failures and repeated tokens cannot publish partial success", async () => {
  await assert.rejects(collectPolicySnapshot(async token => {
    if (token) throw new Error("permission denied");
    return { items: [violation("first")], summary: summary(), nextPageToken: "next" };
  }), /permission denied/);
  await assert.rejects(collectPolicySnapshot(async () => ({ items: [violation("first")], summary: summary(), nextPageToken: "same" })), /repeated pagination token/);
});

test("absent collection metadata stays unknown and cancellation stops loading", async () => {
  const result = await collectPolicySnapshot(async () => ({ items: [], nextPageToken: "" }));
  assert.equal(result.summary, undefined);
  const controller = new AbortController();
  controller.abort();
  await assert.rejects(collectPolicySnapshot(async () => { throw new Error("must not load"); }, controller.signal), { name: "AbortError" });
});

test("policy identity distinguishes scope and engine; filter values avoid all sentinel collisions", () => {
  const a = violation("a");
  const b = violation("b"); b.policy!.namespace = "tenant-b";
  const c = violation("c"); c.engine = "gatekeeper"; c.resource!.kind = "all";
  const rows = [a, b, c];
  assert.equal(policyFilterOptions(rows, "policy", "all").length, 4);
  assert.deepEqual(filterPolicyRows(rows, { ...emptyPolicyFilters, policy: `value:${policyFilterValue(b, "policy")}` }).map(row => row.id), ["b"]);
  assert.deepEqual(filterPolicyRows(rows, { ...emptyPolicyFilters, kind: "value:all" }).map(row => row.id), ["c"]);
  assert.ok(policyFilterOptions(rows, "kind", "value:Job").some(option => option.value === "value:Job"));
});

test("report failures do not become denied events; event search includes reported reason/action", () => {
  const row = violation("failed-report");
  assert.equal(filterPolicyRows([row], { ...emptyPolicyFilters, search: "enforce" }).length, 0);
  assert.equal(row.enforcement, "unknown");
  const event = create(ClusterViolationEventSchema, { id: "event", reason: "PolicyViolation", action: "Resource Blocked", count: 3 });
  assert.equal(filterPolicyRows([event], { ...emptyPolicyFilters, search: "blocked policyviolation" }).length, 1);
  assert.equal(event.count, 3);
});

test("policies differing only by apiVersion get distinct labels; missing selection label is parsed, not raw JSON", () => {
  const a = violation("a"); a.policy!.apiVersion = "kyverno.io/v1";
  const b = violation("b"); b.policy!.apiVersion = "policies.kyverno.io/v1";
  const labels = policyFilterOptions([a, b], "policy", "all").slice(1).map(option => option.label);
  assert.equal(new Set(labels).size, 2);
  assert.ok(labels.some(label => label.includes("kyverno.io/v1")));
  const missing = policyFilterOptions([], "policy", `value:${policyFilterValue(a, "policy")}`).at(-1)!.label;
  assert.ok(!missing.includes("["), missing);
  assert.ok(missing.includes("Policy/tenant-a/require-label") && missing.endsWith("(no loaded rows)"), missing);
});

test("stale snapshot label is separate from incomplete coverage", () => {
  const utc = () => "2026-11-19 02:00:00 UTC";
  assert.equal(staleSnapshotLabel(create(ClusterPolicySnapshotSummarySchema, { incomplete: true }), utc), undefined);
  assert.equal(staleSnapshotLabel(create(ClusterPolicySnapshotSummarySchema, { stale: true, collectedAt: { seconds: 1n } }), utc), "Snapshot is stale — last collected 2026-11-19 02:00:00 UTC");
  assert.match(staleSnapshotLabel(create(ClusterPolicySnapshotSummarySchema, { stale: true }), utc)!, /collection time unknown/);
});
