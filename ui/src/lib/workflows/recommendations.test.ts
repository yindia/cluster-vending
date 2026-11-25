import assert from "node:assert/strict";
import { test } from "node:test";
import { create, type MessageInitShape } from "@bufbuild/protobuf";
import {
  ClusterRecommendationsSchema, RecommendationSource, ResourceRecommendationSchema, type ResourceRecommendation,
} from "../../gen/cloud/v1/recommendation_pb";
import {
  ageText, amount, changeView, DEFAULT_FILTERS, emptyReason, isActionable, isFiltered, matchesFilters, memoryAmount, metricsView, netChange, netChangeNote, netChangeText,
  parseNotices, percentText, sortRecommendations, utcTime, vpaView,
} from "./recommendations";

type Init = Omit<MessageInitShape<typeof ResourceRecommendationSchema>, "$typeName">;
const MIB = 1n << 20n;
const live = "the metrics API did not answer: failed to get API group resources: unable to retrieve the complete list of server APIs: metrics.k8s.io/v1beta1: the server is currently unable to handle the request; recording usage: first recommendations after a day, reliable after a week results unavailable: access to the results bucket was denied";

function row(overrides: Init = {}): ResourceRecommendation {
  return create(ResourceRecommendationSchema, {
    namespace: "payments", workloadKind: "Deployment", workloadName: "api", container: "server", replicas: 2,
    currentRequests: { cpuMillis: 500n, memoryBytes: 512n * MIB },
    recommendedRequests: { cpuMillis: 250n, memoryBytes: 256n * MIB },
    difference: { cpuMillis: -250n, memoryBytes: -256n * MIB },
    cpuPercent: -50, memoryPercent: -50, source: RecommendationSource.USAGE_ESTIMATE, ...overrides,
  });
}
const noData = (overrides: Init = {}) => row({ recommendedRequests: undefined, difference: undefined, cpuPercent: 0, memoryPercent: 0, source: RecommendationSource.NO_DATA, ...overrides });

test("the live combined message splits into separate notices with their own severity", () => {
  const notices = parseNotices(create(ClusterRecommendationsSchema, { message: live, windowDays: 7 }));
  assert.deepEqual(notices.map(notice => [notice.kind, notice.status]), [["metrics", "error"], ["recording", "info"], ["results", "warning"]]);
  assert.match(notices[0].raw, /^the metrics API did not answer: .*handle the request$/);
  assert.equal(notices[1].raw, "recording usage: first recommendations after a day, reliable after a week");
  assert.match(notices[1].description, /7-day window/);
  assert.equal(notices[2].raw, "results unavailable: access to the results bucket was denied");
  assert.match(notices[2].description, /^Access to the results bucket was denied\./);
});

test("an unknown message is one generic notice and an empty one is none", () => {
  assert.deepEqual(parseNotices(create(ClusterRecommendationsSchema, {})), []);
  assert.deepEqual(parseNotices(undefined), []);
  const [notice] = parseNotices(create(ClusterRecommendationsSchema, { message: "something new happened" }));
  assert.equal(notice.kind, "other");
  assert.equal(notice.description, "Something new happened");
});

test("leading unknown text keeps its own notice ahead of a known one", () => {
  const notices = parseNotices(create(ClusterRecommendationsSchema, { message: "odd note; 5 containers are not recorded (only the 100 largest consumers are)" }));
  assert.deepEqual(notices.map(notice => notice.kind), ["other", "truncated"]);
  assert.equal(notices[0].raw, "odd note");
  assert.match(notices[1].description, /^5 containers are not recorded/);
});

test("a registered metrics API that did not answer is shown as not answering, not available", () => {
  const notices = parseNotices(create(ClusterRecommendationsSchema, { message: live }));
  assert.deepEqual(metricsView(create(ClusterRecommendationsSchema, { metricsAvailable: true }), notices).label, "Registered · not answering");
  assert.equal(metricsView(create(ClusterRecommendationsSchema, { metricsAvailable: true }), []).label, "Answering");
  assert.equal(metricsView(create(ClusterRecommendationsSchema, {}), []).label, "Not installed");
  assert.equal(metricsView(undefined, []).label, "Unknown");
  assert.equal(vpaView(create(ClusterRecommendationsSchema, { vpaAvailable: true }), []).label, "Installed");
  assert.equal(vpaView(create(ClusterRecommendationsSchema, {}), parseNotices(create(ClusterRecommendationsSchema, { message: "the Vertical Pod Autoscalers could not be listed: denied" }))).tone, "warning");
});

test("the empty state explains the first reason that applies", () => {
  const notices = parseNotices(create(ClusterRecommendationsSchema, { message: live }));
  assert.equal(emptyReason(notices, true).title, "No usage is being recorded");
  assert.match(emptyReason(notices, true).description, /after a day and are reliable after a week/);
  assert.equal(emptyReason([], false).title, "No sample recorded yet");
  assert.equal(emptyReason(parseNotices(create(ClusterRecommendationsSchema, { message: "recording usage: x" })), true).title, "Recording usage");
});

test("a change cell shows current to recommended with a signed percent", () => {
  assert.deepEqual(changeView(row(), "cpuMillis"), { value: "500 mCPU → 250 mCPU", detail: "−50%", tone: "success" });
  assert.deepEqual(changeView(row({ cpuPercent: 150, recommendedRequests: { cpuMillis: 1250n, memoryBytes: 1n } }), "cpuMillis").detail, "+150%");
  assert.deepEqual(changeView(row({ currentRequests: { cpuMillis: 0n, memoryBytes: 0n } }), "cpuMillis"), { value: "Not set → 250 mCPU", detail: "New request", tone: "warning" });
  assert.deepEqual(changeView(noData(), "memoryBytes"), { value: "512 MiB", detail: "Current · no data", tone: "neutral" });
  assert.equal(changeView(noData({ currentRequests: undefined }), "cpuMillis").value, "Not set");
  assert.equal(percentText(row(), "cpuMillis"), "-50%");
  assert.equal(percentText(noData(), "cpuMillis"), "No recommendation");
});

test("amounts keep bigint precision and mark truncated memory", () => {
  assert.equal(amount(1500n, "cpuMillis"), "1,500 mCPU");
  assert.equal(amount(-250n, "cpuMillis"), "−250 mCPU");
  assert.equal(memoryAmount(512n * MIB), "512 MiB");
  assert.equal(memoryAmount(1000n), "1,000 B");
  assert.equal(memoryAmount((1n << 30n) + 1n), "≈ 1 GiB");
  assert.equal(memoryAmount(-(3n * MIB / 2n)), "−1.5 MiB");
});

test("actionable rows sort first by the largest change, then insufficient data, then no data", () => {
  const rows = [
    noData({ workloadName: "a" }), row({ workloadName: "small", cpuPercent: -10, memoryPercent: 5 }), noData({ source: RecommendationSource.INSUFFICIENT_DATA, workloadName: "b" }), row({ workloadName: "big", cpuPercent: 200 }),
  ].map((recommendation, id) => ({ id, recommendation }));
  assert.deepEqual(sortRecommendations(rows).map(item => item.recommendation.workloadName), ["big", "small", "b", "a"]);
  assert.equal(isActionable(noData()), false);
});

test("filters default to rows with a recommendation and report when they are changed", () => {
  assert.equal(matchesFilters(noData(), DEFAULT_FILTERS), false);
  assert.equal(matchesFilters(noData(), { ...DEFAULT_FILTERS, show: "all" }), true);
  assert.equal(matchesFilters(noData(), { ...DEFAULT_FILTERS, show: `source:${RecommendationSource.NO_DATA}` }), true);
  assert.equal(matchesFilters(row(), { ...DEFAULT_FILTERS, show: `source:${RecommendationSource.NO_DATA}` }), false);
  assert.equal(matchesFilters(row(), { ...DEFAULT_FILTERS, kind: "kind:StatefulSet" }), false);
  assert.equal(matchesFilters(row(), { ...DEFAULT_FILTERS, search: "SERV" }), true);
  assert.equal(matchesFilters(row(), { ...DEFAULT_FILTERS, namespace: "other" }), false);
  assert.equal(isFiltered(DEFAULT_FILTERS, false), false);
  assert.equal(isFiltered(DEFAULT_FILTERS, true), true);
  assert.equal(isFiltered({ ...DEFAULT_FILTERS, show: "all" }, false), true);
});

test("net change sums the differences across replicas", () => {
  const change = netChange([row(), row({ replicas: 1, difference: { cpuMillis: 100n, memoryBytes: 0n } }), noData()], "cpuMillis");
  assert.deepEqual(change, { total: -400n, higher: 1, lower: 1 });
  assert.equal(netChangeText(change, "cpuMillis"), "−400 mCPU");
  assert.equal(netChangeNote(change), "1 lower · 1 higher · all replicas");
  assert.equal(netChangeText(netChange([], "memoryBytes"), "memoryBytes"), "No net change");
  assert.equal(netChangeNote(netChange([], "memoryBytes")), "No recommended change");
});

test("times show in UTC to the minute and ages are relative", () => {
  const at = { seconds: 1_795_000_000n, nanos: 0 } as never;
  assert.equal(utcTime(at, "never"), "2026-11-18 11:06 UTC");
  assert.equal(utcTime(undefined, "never"), "never");
  const now = 1_795_000_000n * 1_000_000_000n;
  assert.equal(ageText(at, now), "Just now");
  assert.equal(ageText(at, now + 12n * 60n * 1_000_000_000n), "12 min ago");
  assert.equal(ageText(at, now + 3n * 3600n * 1_000_000_000n), "3 h ago");
  assert.equal(ageText(undefined, now), "Never");
});
