import assert from "node:assert/strict";
import test from "node:test";
import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { ReleaseChannelInfoSchema, ReleaseMinorSchema } from "../../gen/cloud/v1/admin_release_pb";
import { ReleaseChannel } from "../../gen/cloud/v1/cluster_pb";
import {
  extendedSupportFields, extendedSupportIssue, findVersionOption, isExtendedVersion, kubernetesVersionOptions, minorOf, offerChannel, offeredRelease,
  versionOptionDescription, versionOptionLabel,
} from "./kubernetes-versions";

const minor = (value: string, channel = ReleaseChannel.UNSPECIFIED, extra: Record<string, unknown> = {}) => create(ReleaseMinorSchema, { minor: value, channel, ...extra });
const minors = [
  minor("1.33", ReleaseChannel.STABLE, { default: true, endOfSupport: timestampFromDate(new Date("2026-12-01T00:00:00Z")) }),
  minor("1.31", ReleaseChannel.UNSPECIFIED, { endOfSupport: timestampFromDate(new Date("2026-11-30T00:00:00Z")) }),
  minor("1.35", ReleaseChannel.ALPHA), minor("1.34", ReleaseChannel.BETA),
];
const options = kubernetesVersionOptions(minors);
const manual = { upgradeMode: "1", awsSupportType: "", kubernetesVersion: "" };

test("options are newest first and a minor on no channel is extended support", () => {
  assert.deepEqual(options.map((item) => [item.minor, item.extended]), [["1.35", false], ["1.34", false], ["1.33", false], ["1.31", true]]);
  assert.equal(versionOptionLabel(options[2]), "1.33 · Stable channel · default");
  assert.equal(versionOptionLabel(options[3]), "1.31 · Extended support");
  assert.equal(versionOptionDescription(options[2]), "Support ends 2026-12-01 (UTC).");
  assert.match(versionOptionDescription(options[3]), /^Support ends 2026-11-30 \(UTC\)\. Manual upgrades and the Extended support policy only\.$/);
});

test("a release that puts no minor on a channel has no extended minors", () => {
  const unassigned = kubernetesVersionOptions([minor("1.33"), minor("1.32")]);
  assert.equal(unassigned.some((item) => item.extended), false);
  assert.equal(versionOptionLabel(unassigned[0]), "1.33 · No channel");
});

test("versions match by minor, so a patch or v prefix finds the option", () => {
  assert.equal(minorOf("1.31.4"), "1.31");
  assert.equal(minorOf("v1.31"), "1.31");
  assert.equal(minorOf(" custom "), "custom");
  assert.equal(findVersionOption(options, "1.31.4")?.minor, "1.31");
  assert.equal(findVersionOption(options, ""), undefined);
  assert.equal(isExtendedVersion(options, "1.31"), true);
  assert.equal(isExtendedVersion(options, "1.34"), false);
  assert.equal(isExtendedVersion(options, "1.20"), false);
});

test("the offer comes from the cluster's channel, else stable", () => {
  const channels = [ReleaseChannel.ALPHA, ReleaseChannel.BETA, ReleaseChannel.STABLE].map((channel) => create(ReleaseChannelInfoSchema, { channel, release: `release-${channel}` }));
  assert.equal(offerChannel("0"), ReleaseChannel.STABLE);
  assert.equal(offerChannel("1"), ReleaseChannel.ALPHA);
  assert.equal(offeredRelease(channels, "2"), `release-${ReleaseChannel.BETA}`);
  assert.equal(offeredRelease(channels, ""), `release-${ReleaseChannel.STABLE}`);
  assert.equal(offeredRelease([create(ReleaseChannelInfoSchema, { channel: ReleaseChannel.STABLE })], "0"), undefined);
});

test("an extended minor refuses Automatic upgrades and the Standard support policy, as the server does", () => {
  assert.equal(extendedSupportIssue({ ...manual, kubernetesVersion: "1.31" }, options), undefined);
  assert.equal(extendedSupportIssue({ ...manual, kubernetesVersion: "1.31", awsSupportType: "2" }, options), undefined);
  assert.match(extendedSupportIssue({ ...manual, kubernetesVersion: "1.31", awsSupportType: "1" }, options) ?? "", /Standard support policy/);
  assert.match(extendedSupportIssue({ ...manual, kubernetesVersion: "1.31", upgradeMode: "2" }, options) ?? "", /Manual upgrades only/);
  assert.equal(extendedSupportIssue({ ...manual, kubernetesVersion: "1.34", awsSupportType: "1", upgradeMode: "2" }, options), undefined);
});

test("an edit with no version pin is checked against the minor the cluster runs", () => {
  assert.match(extendedSupportIssue({ ...manual, upgradeMode: "2" }, options, "1.31.7") ?? "", /Manual upgrades only/);
  assert.equal(extendedSupportIssue({ ...manual, upgradeMode: "2", kubernetesVersion: "1.34" }, options, "1.31.7"), undefined);
});

test("choosing an extended minor sets Manual upgrades and Extended support", () => {
  assert.deepEqual(extendedSupportFields, { upgradeMode: "1", awsSupportType: "2" });
  assert.equal(extendedSupportIssue({ ...extendedSupportFields, kubernetesVersion: "1.31" }, options), undefined);
});
