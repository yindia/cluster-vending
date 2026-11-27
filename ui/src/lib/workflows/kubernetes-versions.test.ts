import assert from "node:assert/strict";
import test from "node:test";
import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { ReleaseChannelInfoSchema, ReleaseMinorSchema } from "../../gen/cloud/v1/admin_release_pb";
import { ReleaseChannel, UpgradeMode } from "../../gen/cloud/v1/cluster_pb";
import { AwsSupportType } from "../../gen/cloud/v1/aws_pb";
import { extendedSupportIssue, findVersionOption, kubernetesVersionOptions, minorOf, offerChannel, offeredRelease } from "./kubernetes-versions";

const minor = (value: string, channel = ReleaseChannel.UNSPECIFIED, extra: Record<string, unknown> = {}) => create(ReleaseMinorSchema, { minor: value, channel, ...extra });
const minors = [
  minor("1.33", ReleaseChannel.STABLE, { default: true, endOfSupport: timestampFromDate(new Date("2026-12-01T00:00:00Z")) }),
  minor("1.31", ReleaseChannel.UNSPECIFIED, { endOfSupport: timestampFromDate(new Date("2026-11-30T00:00:00Z")) }),
  minor("1.35", ReleaseChannel.ALPHA), minor("1.34", ReleaseChannel.BETA),
];
const options = kubernetesVersionOptions(minors);
const manual = { upgradeMode: UpgradeMode.MANUAL, supportType: AwsSupportType.UNSPECIFIED, kubernetesVersion: "" };

test("options are newest first and a minor on no channel is extended support", () => {
  assert.deepEqual(options.map((item) => [item.minor, item.extended]), [["1.35", false], ["1.34", false], ["1.33", false], ["1.31", true]]);
  assert.equal(options[2].isDefault, true);
});

test("a release that puts no minor on a channel has no extended minors", () => {
  const unassigned = kubernetesVersionOptions([minor("1.33"), minor("1.32")]);
  assert.equal(unassigned.some((item) => item.extended), false);
});

test("versions match by minor, so a patch or v prefix finds the option", () => {
  assert.equal(minorOf("1.31.4"), "1.31");
  assert.equal(minorOf("v1.31"), "1.31");
  assert.equal(minorOf(" custom "), "custom");
  assert.equal(findVersionOption(options, "1.31.4")?.minor, "1.31");
  assert.equal(findVersionOption(options, ""), undefined);
  assert.equal(findVersionOption(options, "1.31")?.extended, true);
  assert.equal(findVersionOption(options, "1.34")?.extended, false);
  assert.equal(findVersionOption(options, "1.20"), undefined);
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
  assert.equal(extendedSupportIssue({ ...manual, kubernetesVersion: "1.31", supportType: AwsSupportType.EXTENDED }, options), undefined);
  assert.match(extendedSupportIssue({ ...manual, kubernetesVersion: "1.31", supportType: AwsSupportType.STANDARD }, options) ?? "", /AWS_SUPPORT_TYPE_STANDARD/);
  assert.match(extendedSupportIssue({ ...manual, kubernetesVersion: "1.31", upgradeMode: UpgradeMode.AUTOMATIC }, options) ?? "", /UPGRADE_MODE_MANUAL only/);
  assert.equal(extendedSupportIssue({ ...manual, kubernetesVersion: "1.34", supportType: AwsSupportType.STANDARD, upgradeMode: UpgradeMode.AUTOMATIC }, options), undefined);
});

test("an edit with no version pin is checked against the minor the cluster runs", () => {
  assert.match(extendedSupportIssue({ ...manual, upgradeMode: UpgradeMode.AUTOMATIC }, options, "1.31.7") ?? "", /UPGRADE_MODE_MANUAL only/);
  assert.equal(extendedSupportIssue({ ...manual, upgradeMode: UpgradeMode.AUTOMATIC, kubernetesVersion: "1.34" }, options, "1.31.7"), undefined);
});
