import { expect, test, type Page } from "@playwright/test";

const releaseName = "release-version-filter";
const minors = [
  { minor: "1.34", components: [{ name: "external-dns", version: "0.16.0" }, { name: "ingress", version: "1.0.0" }] },
  { minor: "1.35", components: [{ name: "external-dns", version: "0.17.0" }] },
  { minor: "1.36", components: [] },
];

async function mockReleases(page: Page) {
  let supported = minors;
  const calls: string[] = [];
  const unexpected: string[] = [];
  await page.route("**/api/rpc/**", async route => {
    const method = new URL(route.request().url()).pathname.split("/").at(-1)!;
    calls.push(method);
    let response: object;
    switch (method) {
      case "WhoAmI": response = { anonymous: true }; break;
      case "ListTenants": response = { tenants: [{ id: "acme" }] }; break;
      case "GetRelease": response = { release: { name: releaseName, valid: true, kubernetes: supported } }; break;
      case "ListReleaseChannels": response = { channels: [] }; break;
      case "GetReleaseChannel": response = { channel: { channel: "RELEASE_CHANNEL_STABLE", release: releaseName, kubernetesMinor: "1.34" } }; break;
      default:
        unexpected.push(method);
        await route.fulfill({ status: 501, contentType: "application/json", body: JSON.stringify({ code: "unimplemented", message: `Unmocked RPC: ${method}` }) });
        return;
    }
    await route.fulfill({ contentType: "application/json", body: JSON.stringify(response) });
  });
  return { calls, unexpected, removeMinor: (minor: string) => { supported = supported.filter(item => item.minor !== minor); } };
}

async function selectVersion(page: Page, version: string) {
  await page.getByRole("combobox", { name: "Kubernetes version", exact: true }).click();
  await page.getByRole("option", { name: version, exact: true }).click();
}

test("release Kubernetes versions filter both inventories and compose with component search", async ({ page }) => {
  const mock = await mockReleases(page);
  await page.goto(`/releases/${releaseName}`);
  const selector = page.getByRole("combobox", { name: "Kubernetes version", exact: true });
  await expect(selector).toContainText("All versions");
  const loadedRequests = mock.calls.filter(method => method === "GetRelease").length;
  await selector.click();
  await expect(page.getByRole("option")).toHaveText(["All versions", "1.34", "1.35", "1.36"]);
  await page.getByRole("option", { name: "1.35", exact: true }).click();
  const support = page.getByRole("table", { name: "Supported Kubernetes minors", exact: true });
  await expect(support).toContainText("1.35");
  await expect(support).not.toContainText("1.34");
  await expect(page.getByRole("status").filter({ hasText: "1 of 3 Kubernetes versions" })).toBeVisible();
  await page.getByRole("tab", { name: "Components", exact: true }).click();
  const pins = page.getByRole("table", { name: "Release component pins by Kubernetes minor", exact: true });
  await expect(pins).toContainText("0.17.0");
  await expect(pins).not.toContainText("0.16.0");
  const search = page.getByRole("searchbox", { name: "Search Release component pins by Kubernetes minor", exact: true });
  await search.fill("ingress");
  await expect(pins).toContainText("No rows match this search");
  await selectVersion(page, "All versions");
  await expect(search).toHaveValue("ingress");
  await expect(pins).toContainText("ingress");
  await expect(pins).not.toContainText("external-dns");
  await selectVersion(page, "1.36");
  await expect(search).toHaveValue("ingress");
  await search.fill("");
  await expect(pins).toContainText("No platform component pins returned");
  await selectVersion(page, "All versions");
  await expect(pins).toContainText("0.16.0");
  await expect(pins).toContainText("0.17.0");
  expect(mock.calls.filter(method => method === "GetRelease")).toHaveLength(loadedRequests);
  expect(mock.unexpected).toEqual([]);
});

test("removed Kubernetes selections reset on release refresh", async ({ page }) => {
  const mock = await mockReleases(page);
  await page.goto(`/releases/${releaseName}`);
  await page.getByRole("tab", { name: "Components", exact: true }).click();
  await selectVersion(page, "1.35");
  const search = page.getByRole("searchbox", { name: "Search Release component pins by Kubernetes minor", exact: true });
  await search.fill("external-dns");
  mock.removeMinor("1.35");
  await page.getByRole("button", { name: "Refresh release details", exact: true }).click();
  await expect(page.getByRole("combobox", { name: "Kubernetes version", exact: true })).toContainText("All versions");
  await expect(search).toHaveValue("external-dns");
  const pins = page.getByRole("table", { name: "Release component pins by Kubernetes minor", exact: true });
  await expect(pins).toContainText("0.16.0");
  await expect(pins).not.toContainText("0.17.0");
  await page.getByRole("combobox", { name: "Kubernetes version", exact: true }).click();
  await expect(page.getByRole("option")).toHaveText(["All versions", "1.34", "1.36"]);
  expect(mock.unexpected).toEqual([]);
});

test("channel component pins keep the exact offered minor and a right aligned refresh", async ({ page }) => {
  const mock = await mockReleases(page);
  await page.goto("/channels/stable");
  await page.getByRole("tab", { name: "Components", exact: true }).click();
  const pins = page.getByRole("table", { name: "Release component pins by Kubernetes minor", exact: true });
  await expect(pins).toContainText("0.16.0");
  await expect(pins).not.toContainText("0.17.0");
  await expect(page.getByRole("combobox", { name: "Kubernetes version", exact: true })).toHaveCount(0);
  await expect(page.getByRole("heading", { name: "AWS EKS add-on versions", exact: true })).toHaveCount(0);
  const refresh = page.getByRole("button", { name: "Refresh offered component pins", exact: true });
  for (const width of [1440, 375]) {
    await page.setViewportSize({ width, height: 1000 });
    await expect(async () => {
      const button = await refresh.boundingBox();
      const table = await pins.boundingBox();
      expect(button).not.toBeNull();
      expect(table).not.toBeNull();
      expect(Math.abs(button!.x + button!.width - table!.x - table!.width)).toBeLessThan(2);
    }).toPass();
  }
  expect(mock.unexpected).toEqual([]);
});
