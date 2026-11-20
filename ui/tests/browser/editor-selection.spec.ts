import { expect, test, type Locator, type Page } from "@playwright/test";

const clusterId = "00000000-0000-4000-8000-000000000003";
const values = { replicaCount: 2, policy: "sync", enabled: true, nodeSelector: { role: "worker" }, note: "Selection must remain readable" };

async function mockEditorReads(page: Page) {
  const unexpected: string[] = [];
  await page.addInitScript(() => {
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText: async (value: string) => { (window as unknown as { copiedEditorText: string }).copiedEditorText = value; } } });
  });
  await page.route("**/api/rpc/**", async route => {
    const method = new URL(route.request().url()).pathname.split("/").at(-1)!;
    const component = { id: "00000000-0000-4000-8000-000000000004", tenantId: "acme", clusterId, component: "external-dns", etag: "editor-etag", values, management: "COMPONENT_MANAGEMENT_TENANT", version: "0.16.0", status: { phase: "COMPONENT_PHASE_READY" } };
    let response: object;
    switch (method) {
      case "WhoAmI": response = { subject: "editor-reader", grants: [{ role: "ACCESS_ROLE_TENANT_ADMIN", tenants: ["acme"] }] }; break;
      case "ListTenants": response = { tenants: [{ id: "acme" }] }; break;
      case "ListCloudAccounts": response = { accounts: [] }; break;
      case "ListNetworks": response = { networks: [] }; break;
      case "GetCluster": response = { cluster: { id: clusterId, tenantId: "acme", name: "production", spec: {}, status: { state: "CLUSTER_STATE_RUNNING" } } }; break;
      case "ListClusters": response = { clusters: [] }; break;
      case "GetReleaseChannel": response = { channel: { channel: "RELEASE_CHANNEL_STABLE", release: "selection-release", kubernetesMinor: "1.35" } }; break;
      case "GetRelease": response = { release: { name: "selection-release", valid: true, kubernetes: [{ minor: "1.35", components: [{ name: "external-dns", version: "0.16.0", values }] }] } }; break;
      case "ListComponentDefinitions": response = { definitions: [{ name: "external-dns", management: "COMPONENT_MANAGEMENT_TENANT", provider: "CLOUD_PROVIDER_AWS", configurable: true, versions: { "1.35": "0.16.0" } }] }; break;
      case "ListComponents": response = { components: [component] }; break;
      case "GetComponent": response = { component }; break;
      default:
        unexpected.push(method);
        await route.fulfill({ status: 501, contentType: "application/json", body: JSON.stringify({ code: "unimplemented", message: `Unexpected RPC: ${method}` }) });
        return;
    }
    await route.fulfill({ contentType: "application/json", body: JSON.stringify(response) });
  });
  return unexpected;
}

async function setAppearance(page: Page, mode: "Light" | "Dark") {
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  const settings = page.getByRole("dialog", { name: "Settings", exact: true });
  await settings.getByText(mode, { exact: true }).click();
  await settings.getByRole("button", { name: "Close", exact: true }).last().click();
}

async function contrast(editor: Locator) {
  return editor.evaluate(el => {
    const cm = el.closest(".cm-editor")!;
    const overlay = cm.querySelector(".cm-selectionBackground")!;
    const nodes = [...el.querySelectorAll(".cm-line, .cm-line span")];
    const luminance = (color: string) => {
      const rgb = color.match(/[\d.]+/g)!.slice(0, 3).map(Number).map(value => {
        const channel = value / 255;
        return channel <= 0.04045 ? channel / 12.92 : ((channel + 0.055) / 1.055) ** 2.4;
      });
      return rgb[0] * 0.2126 + rgb[1] * 0.7152 + rgb[2] * 0.0722;
    };
    const ratio = (a: string, b: string) => {
      const x = luminance(a), y = luminance(b);
      return (Math.max(x, y) + 0.05) / (Math.min(x, y) + 0.05);
    };
    const background = getComputedStyle(overlay).backgroundColor;
    const native = nodes.map(node => {
      const style = getComputedStyle(node, "::selection");
      return { background: style.backgroundColor, ratio: ratio(style.color, style.backgroundColor) };
    });
    // Secondary CodeMirror ranges retain syntax colors rather than native
    // selected-text colors, so those must contrast with the overlay too.
    return { background, native, syntaxRatios: nodes.map(node => ratio(getComputedStyle(node).color, background)) };
  });
}

for (const mode of ["Light", "Dark"] as const) {
  for (const scenario of ["readonly Helm values", "editable Helm values", "YAML manifest"] as const) {
    test(`${scenario} selection stays readable when focused and blurred in ${mode.toLowerCase()} mode`, async ({ page }, testInfo) => {
      const unexpected = await mockEditorReads(page);
      let editor: Locator;
      let copyLabel: string;
      if (scenario === "readonly Helm values") {
        await page.goto("/channels/stable");
        await setAppearance(page, mode);
        await page.getByRole("tab", { name: "Components", exact: true }).click();
        await page.getByRole("button", { name: "Inspect locked values", exact: true }).click();
        copyLabel = "Locked Helm values for external-dns on Kubernetes 1.35";
        editor = page.getByRole("textbox", { name: copyLabel, exact: true });
      } else if (scenario === "editable Helm values") {
        await page.goto(`/clusters/${clusterId}?tab=components`);
        await setAppearance(page, mode);
        await page.getByRole("button", { name: "Manage component", exact: true }).click();
        copyLabel = "Tenant values (YAML object)";
        editor = page.getByRole("textbox", { name: copyLabel, exact: true });
      } else {
        await page.goto("/networks");
        await setAppearance(page, mode);
        await page.getByRole("button", { name: "Import network", exact: true }).click();
        await page.getByRole("radio", { name: "Paste YAML", exact: true }).click();
        copyLabel = "NetworkImport manifest";
        editor = page.getByRole("textbox", { name: copyLabel, exact: true });
        await editor.fill("apiVersion: cluster-vending.io/v1\nkind: NetworkImport\nmetadata:\n  name: selected-network\nspec:\n  enabled: true\n  count: 2\n");
      }
      await expect(editor).toBeVisible();
      const original = await editor.textContent();
      await editor.click();
      await editor.press(process.platform === "darwin" ? "Meta+A" : "Control+A");
      const selected = await page.evaluate(() => String(window.getSelection()));
      expect(selected).toContain(scenario === "YAML manifest" ? "selected-network" : "replicaCount");
      expect(selected.split("\n").length).toBeGreaterThan(3);
      for (const focused of [true, false]) {
        if (!focused) await editor.evaluate(el => (el as HTMLElement).blur());
        await expect.poll(async () => {
          const colors = await contrast(editor);
          return Math.min(...colors.native.map(value => value.ratio), ...colors.syntaxRatios);
        }).toBeGreaterThanOrEqual(4.5);
        const colors = await contrast(editor);
        expect(colors.native.every(value => value.background === colors.background)).toBe(true);
        await editor.locator("..").screenshot({ path: testInfo.outputPath(`${focused ? "focused" : "unfocused"}.png`) });
      }
      // A browser DOM range gets the same paired ::selection colors as the
      // editor's keyboard selection; no decorations rewrite the document.
      await editor.evaluate(el => { const range = document.createRange(); range.selectNodeContents(el); const selection = window.getSelection()!; selection.removeAllRanges(); selection.addRange(range); });
      expect(Math.min(...(await contrast(editor)).native.map(value => value.ratio))).toBeGreaterThanOrEqual(4.5);
      await expect(editor).toHaveText(original!);
      await page.getByRole("button", { name: `Copy ${copyLabel}`, exact: true }).click();
      await expect.poll(() => page.evaluate(() => (window as unknown as { copiedEditorText?: string }).copiedEditorText)).toContain(scenario === "YAML manifest" ? "selected-network" : "replicaCount");
      await expect(editor).toHaveText(original!);
      if (scenario === "readonly Helm values") {
        await expect(editor).toHaveAttribute("aria-readonly", "true");
        await editor.click();
        await editor.press("x");
        await expect(editor).toHaveText(original!);
      }
      expect(unexpected).toEqual([]);
    });
  }
}
