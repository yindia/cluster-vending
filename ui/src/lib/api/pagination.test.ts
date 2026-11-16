import assert from "node:assert/strict";
import { test } from "node:test";
import { collectPages } from "./pagination";

test("pagination collects all pages and refuses repeated tokens", async () => {
  assert.deepEqual(await collectPages(async (token) => token ? { items: [2], nextPageToken: "" } : { items: [1], nextPageToken: "second" }), [1, 2]);
  await assert.rejects(collectPages(async () => ({ items: [1], nextPageToken: "repeated" })), /repeated pagination token/);
  await assert.rejects(collectPages(async () => ({ items: [], nextPageToken: "" }), AbortSignal.abort()), /abort/i);
});
