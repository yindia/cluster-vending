import { expect, test, type Page, type Locator } from "@playwright/test";
import { stringify } from "yaml";

const accountId = "00000000-0000-4000-8000-000000000001";
const networkId = "00000000-0000-4000-8000-000000000002";
const clusterId = "00000000-0000-4000-8000-000000000003";
const account = { id: accountId, tenantId: "acme", accountName: "Production", aws: { accountId: "111122223333", role: "cluster-vending-provisioner" }, status: { state: "CLOUD_ACCOUNT_STATE_READY", managedResources: [{ kind: "ProviderConfig", name: "production-provider", externalName: "production-provider", state: "MANAGED_RESOURCE_STATE_READY", ready: "CONDITION_STATUS_TRUE", synced: "CONDITION_STATUS_TRUE" }], access: { verified: true, checkedTime: "2026-11-16T00:00:00Z", attemptedTime: "2026-11-16T00:00:00Z" } } };
const network = { id: networkId, name: "prod-network", displayName: "Production network", tenantId: "acme", etag: "network-etag", generation: "1", spec: { region: "us-west-1", bootstrapId: accountId, tier: "TIER_1", layout: { cidrBlock: "10.1.0.0/16", availabilityZoneCount: 2, natGatewayMode: "NAT_GATEWAY_MODE_SINGLE" } }, status: { state: "NETWORK_STATE_READY", observedGeneration: "1", clusterIds: [clusterId], managedResources: [{ kind: "VPC", name: "production-vpc", externalName: "vpc-0123456789abcdef0", state: "MANAGED_RESOURCE_STATE_READY", ready: "CONDITION_STATUS_TRUE", synced: "CONDITION_STATUS_TRUE" }] } };
const cluster = { id: clusterId, name: "payments", tenantId: "acme", etag: "cluster-etag", generation: "1", spec: { region: "us-west-1", network: { networkId }, releaseChannel: "RELEASE_CHANNEL_STABLE", upgradeMode: "UPGRADE_MODE_MANUAL", controlPlane: { kubernetesVersion: "1.35", endpointAccess: { publicAccessCidrs: ["203.0.113.0/24"] } }, defaultNodePool: { instanceTypes: ["t3.medium"], diskSizeGb: 20, scalingConfig: { minSize: 2, maxSize: 3 } }, aws: { irsa: true } }, status: { state: "CLUSTER_STATE_RUNNING", currentKubernetesVersion: "1.35", platformRelease: "release-stable", observedGeneration: "1", tier: "CLUSTER_TIER_1", managedResources: [{ kind: "NodeGroup", name: "payments-workers", externalName: "payments-workers-external", state: "MANAGED_RESOURCE_STATE_READY", ready: "CONDITION_STATUS_TRUE", synced: "CONDITION_STATUS_TRUE", paused: true }] } };

const componentId = "00000000-0000-4000-8000-000000000004";
const component = { id: componentId, tenantId: "acme", clusterId, component: "external-dns", etag: "component-etag", generation: "1", management: "COMPONENT_MANAGEMENT_TENANT", version: "0.16.0", values: { replicaCount: 2 }, status: { phase: "COMPONENT_PHASE_READY", observedGeneration: "1", installedVersion: "0.15.0", targetVersion: "0.16.0", clusterName: "payments", release: "release-stable", effectiveValues: { replicaCount: 2, policy: "sync" } } };
const definition = { name: "external-dns", description: "Synchronize DNS records for services and ingress.", management: "COMPONENT_MANAGEMENT_TENANT", provider: "CLOUD_PROVIDER_AWS", configurable: true, valuesSchema: { type: "object" }, versions: { "1.35": "0.16.0" }, platformValues: { policy: "sync" }, userValues: "# Number of replicas\nreplicaCount: 2\n" };
const stableRelease = { name: "release-stable", valid: true, shipped: true, channels: ["RELEASE_CHANNEL_STABLE"], notes: "Stable platform components.", kubernetes: [{ minor: "1.35", channel: "RELEASE_CHANNEL_STABLE", default: true, components: [{ name: "external-dns", version: "0.16.0", values: { policy: "sync" } }] }, { minor: "1.36", channel: "RELEASE_CHANNEL_ALPHA", components: [{ name: "external-dns", version: "0.17.0", values: { policy: "sync" } }] }] };
const stableChannel = { channel: "RELEASE_CHANNEL_STABLE", release: "release-stable", kubernetesMinor: "1.35", promotion: "AUTOMATIC", pending: { release: "release-next" }, rollout: { done: true } };

for (const kind of ["Cluster", "Network"] as const) {
  test(`${kind} list shows effective pause separately from health and filters locally`, async ({ page }) => {
    const calls = await mockAPI(page);
    const base = kind === "Cluster" ? cluster : network;
    const resources = [
      { ...base, id: "00000000-0000-4000-8000-000000000021", name: "policy-paused", displayName: "policy-paused", status: { ...base.status, managedResources: [], freeze: { scope: "tenant", reason: "Maintenance window" } } },
      { ...base, id: "00000000-0000-4000-8000-000000000022", name: "policy-exception", displayName: "policy-exception", status: { ...base.status, managedResources: [], freeze: { scope: "tenant", reason: "Maintenance window", letThrough: true } } },
      { ...base, id: "00000000-0000-4000-8000-000000000023", name: "policy-active", displayName: "policy-active", status: { ...base.status, managedResources: [] } },
    ];
    const method = kind === "Cluster" ? "ListClusters" : "ListNetworks";
    const listCalls: string[] = [];
    await page.route(`**/api/rpc/cloud.v1.${kind}Service/${method}`, route => {
      listCalls.push(method);
      return route.fulfill({ contentType: "application/json", body: JSON.stringify({ [kind === "Cluster" ? "clusters" : "networks"]: resources }) });
    });
    await page.goto(kind === "Cluster" ? "/clusters" : "/networks");
    const table = page.getByRole("table", { name: kind === "Cluster" ? "Clusters" : "Tenant networks", exact: true });
    await expect(table.getByRole("columnheader", { name: /^Pause(?:\s|$)/ })).toBeVisible();
    for (const [name, state] of [["policy-paused", "Paused"], ["policy-exception", "Let through"], ["policy-active", "Not paused"]]) {
      const row = table.getByRole("row").filter({ has: page.getByRole("link", { name, exact: true }) });
      await expect(row.getByText(state, { exact: true })).toBeVisible();
      await expect(row.getByText(state, { exact: true })).toBeInViewport();
      await expect(row.getByText(kind === "Cluster" ? "Running" : "Ready", { exact: true })).toBeVisible();
    }
    await page.screenshot({ path: `/tmp/cv-ui-${kind.toLowerCase()}-pause-list.png`, animations: "disabled" });
    const loaded = listCalls.length;
    for (const [state, name] of [["Paused", "policy-paused"], ["Let through", "policy-exception"], ["Not paused", "policy-active"]]) {
      await page.getByRole("combobox", { name: "Filter by pause", exact: true }).click();
      await page.getByRole("option", { name: state, exact: true }).click();
      await expect(table.getByRole("row")).toHaveCount(2);
      await expect(table.getByRole("link", { name, exact: true })).toBeVisible();
    }
    expect(listCalls).toHaveLength(loaded);
    expect(calls.some(call => /^(GetCluster|GetNetwork|Freeze|Unfreeze)$/.test(call.method))).toBe(false);
  });
}

type Call = { service: string; method: string; body: Record<string, unknown> };

async function advanceToReview(dialog: Locator) {
  for (let step = 0; step < 4; step++) {
    const next = dialog.getByRole("button", { name: "Next", exact: true });
    if (!await next.count()) break;
    await next.click();
  }
  await expect(dialog.getByRole("heading", { name: "Review", exact: true })).toBeVisible();
}

async function mockAPI(page: Page, options: { viewer?: boolean; failApply?: boolean; legacyNetworkNames?: boolean; pagedInventory?: boolean; eventHistory?: boolean; diagnostics?: boolean; componentCatalog?: boolean; neverScanned?: boolean; frozen?: boolean; freezeConverges?: boolean; channelSubscriptions?: boolean; tenantAdmin?: boolean; platformPaused?: boolean; richTables?: boolean; wildcardAdmin?: boolean; importedNetwork?: boolean; importedCluster?: boolean; blockedImport?: boolean; failTenantCreate?: boolean } = {}) {
  const calls: Call[] = [];
  let componentUpdated = false;
  const tenants = new Set(["acme", "other-tenant"]);
  const importPlan = { phase: options.blockedImport ? "IMPORT_PHASE_BLOCKED" : "IMPORT_PHASE_PLANNED", resources: [{ kind: "Cluster", name: "existing-control-plane", externalName: "cloud-cluster", disposition: options.blockedImport ? "DISPOSITION_BLOCKING" : "DISPOSITION_MATCHES", detail: "Existing resources are currently observe-only." }, { kind: "SecurityGroup", externalName: "sg-external", disposition: "DISPOSITION_NOT_IMPORTED", detail: "Another team owns this resource." }] };
  let enabledComponent: Record<string, unknown> | undefined;
  let savedValues: unknown = component.values;
  let freezeRevision = "before-freeze";
  let freezePhase = options.frozen ? "FREEZE_PHASE_FROZEN" : "FREEZE_PHASE_UNFROZEN";
  await page.route("**/api/rpc/**", async (route) => {
    const method = new URL(route.request().url()).pathname.split("/").at(-1)!;
    const body = route.request().postDataJSON() as Record<string, unknown>;
    const service = new URL(route.request().url()).pathname.split("/").at(-2)!;
    calls.push({ service, method, body });
    let response: unknown;
    let status = 200;
    switch (method) {
      case "WhoAmI": response = options.wildcardAdmin ? { subject: "tenant-manager", grants: [{ role: "ACCESS_ROLE_TENANT_ADMIN", tenants: ["*"] }] } : options.viewer ? { subject: "reader", grants: [{ role: "ACCESS_ROLE_TENANT_VIEWER", tenants: ["acme"] }] } : options.tenantAdmin ? { subject: "tenant-admin", grants: [{ role: "ACCESS_ROLE_TENANT_ADMIN", tenants: ["acme"] }] } : { anonymous: true }; break;
      case "ListTenants": response = { tenants: [...tenants].filter(id => !options.viewer && !options.tenantAdmin || id === "acme").map(id => ({ id })), canCreateTenants: !options.viewer }; break;
      case "CreateTenant":
        if (options.failTenantCreate) { status = 403; response = { code: "permission_denied", message: "Tenant creation denied by the API" }; break; }
        tenants.add(String(body.tenantId)); response = { tenant: { id: body.tenantId } }; break;
      case "ImportNetwork": response = { network: { ...network, name: body.name, status: { ...network.status, state: "NETWORK_STATE_IMPORTED", import: { phase: "IMPORT_PHASE_DISCOVERING" } } } }; break;
      case "ImportCluster": response = { cluster: { ...cluster, name: body.name, status: { ...cluster.status, state: "CLUSTER_STATE_IMPORTED", import: { phase: "IMPORT_PHASE_DISCOVERING" } } } }; break;
      case "TakeOverNetwork": response = { network: { ...network, status: { ...network.status, state: "NETWORK_STATE_UPDATING", import: { ...importPlan, phase: "IMPORT_PHASE_CONVERGING" } } } }; break;
      case "TakeOverCluster": response = { cluster: { ...cluster, status: { ...cluster.status, state: "CLUSTER_STATE_UPDATING", import: { ...importPlan, phase: "IMPORT_PHASE_CONVERGING" } } } }; break;
      case "ListCloudAccounts": response = { accounts: body.tenantId === "acme" ? [account] : [] }; break;
      case "GetCloudAccount": response = { account: options.richTables ? { ...account, status: { ...account.status, managedResources: [...account.status.managedResources, { ...account.status.managedResources[0], kind: "Secret", name: "account-credentials", externalName: "account-credentials" }] } } : account }; break;
      case "ListNetworks": response = { networks: body.tenantId === "acme" ? options.pagedInventory && body.pageToken ? [{ ...network, id: "00000000-0000-4000-8000-000000000005", name: "second-network", displayName: "Second page network", spec: { ...network.spec, region: "eu-west-1", tier: "TIER_2" } }] : [network] : [], nextPageToken: options.pagedInventory && !body.pageToken ? "page-2" : "" }; break;
      case "GetNetwork": response = { network: options.legacyNetworkNames ? { ...network, status: { ...network.status, clusterIds: ["payments"] } } : options.richTables ? { ...network, status: { ...network.status, managedResources: [...network.status.managedResources, { ...network.status.managedResources[0], kind: "Subnet", name: "private-subnet", externalName: "subnet-extra" }] } } : network, asRequested: true }; break;
      case "ListClusters": if (options.channelSubscriptions) {
        response = { clusters: body.tenantId !== "acme" ? [] : body.pageToken ? [
          { ...cluster, id: "00000000-0000-4000-8000-000000000011", name: "beta-test", spec: { ...cluster.spec, releaseChannel: "RELEASE_CHANNEL_BETA" } },
          { ...cluster, id: "00000000-0000-4000-8000-000000000012", name: "automatic-stable", spec: { ...cluster.spec, releaseChannel: "RELEASE_CHANNEL_UNSPECIFIED", upgradeMode: "UPGRADE_MODE_AUTOMATIC" } },
        ] : [
          cluster,
          { ...cluster, id: "00000000-0000-4000-8000-000000000009", name: "alpha-test", spec: { ...cluster.spec, releaseChannel: "RELEASE_CHANNEL_ALPHA" } },
          { ...cluster, id: "00000000-0000-4000-8000-000000000010", name: "unassigned-manual", spec: { ...cluster.spec, releaseChannel: "RELEASE_CHANNEL_UNSPECIFIED" } },
        ], nextPageToken: body.tenantId === "acme" && !body.pageToken ? "subscriptions-2" : "" }; break;
      } response = { clusters: body.tenantId === "acme" ? options.pagedInventory && body.pageToken ? [{ ...cluster, id: "00000000-0000-4000-8000-000000000006", name: "second-cluster", spec: { ...cluster.spec, releaseChannel: "RELEASE_CHANNEL_BETA" } }] : [cluster] : [], nextPageToken: options.pagedInventory && !body.pageToken ? "page-2" : "" }; break;
      case "GetCluster": response = { cluster: options.diagnostics ? { ...cluster, status: { ...cluster.status, endpoint: { url: "https://example.eks.amazonaws.com" }, aws: { clusterArn: "arn:aws:eks:us-west-1:111122223333:cluster/payments", nodeGroupArn: "arn:aws:eks:us-west-1:111122223333:nodegroup/payments/workers/example", clusterSecurityGroupId: "sg-example", platformVersion: "eks.1" }, conditions: [{ type: "Ready", status: "CONDITION_STATUS_TRUE", reason: "Reconciled", message: "All resources are current", lastTransitionTime: "2026-11-16T00:00:00Z" }] } } : options.richTables ? { ...cluster, status: { ...cluster.status, managedResources: [...cluster.status.managedResources, { ...cluster.status.managedResources[0], kind: "Release", name: "dns-release", externalName: "dns-release" }] } } : cluster, asRequested: true }; break;
      case "ListComponents": response = { components: options.richTables && body.pageToken ? [{ ...component, id: "00000000-0000-4000-8000-000000000019", component: "ingress", management: "COMPONENT_MANAGEMENT_PLATFORM", status: { ...component.status, phase: "COMPONENT_PHASE_INSTALLING" } }] : [{ ...component, values: savedValues }, ...(enabledComponent ? [enabledComponent] : [])], nextPageToken: options.richTables && !body.pageToken ? "component-page-2" : "" }; break;
      case "GetComponent": if (enabledComponent && body.componentId === enabledComponent.id) { response = { component: enabledComponent }; break; } response = { component: { ...component, values: savedValues, generation: componentUpdated ? "2" : "1", status: { ...component.status, observedGeneration: componentUpdated ? "2" : "1" } } }; break;
      case "ListComponentDefinitions": response = { definitions: [{ ...definition, versions: body.clusterId ? definition.versions : {} }, ...(options.componentCatalog ? [{ ...definition, name: "policies", description: "User configured policies" }, { ...definition, name: "blocked-component", disabled: true, disabledReason: "Disabled by platform policy" }] : [])] }; break;
      case "GetComponentValues": response = { userValues: definition.userValues }; break;
      case "UpdateComponent": {
        const submitted = body.component as { values: unknown };
        if (!body.validateOnly) { componentUpdated = true; savedValues = submitted.values; }
        response = { component: { ...component, values: submitted.values } }; break;
      }
      case "EnableComponent": {
        const submitted = body.component as Record<string, unknown>;
        const candidate = { ...component, ...submitted, id: "00000000-0000-4000-8000-000000000008", tenantId: "acme", clusterId, status: { ...component.status, phase: "COMPONENT_PHASE_PENDING", installedVersion: "", targetVersion: "0.16.0" } };
        if (!body.validateOnly) enabledComponent = candidate;
        response = { component: candidate }; break;
      }
      case "ListClusterDeprecations": response = options.neverScanned ? { findings: [], targetVersion: "1.36", total: 0, removed: 0 } : { findings: [{ namespace: "payments", kind: "PodDisruptionBudget", name: "payments-budget", apiVersion: "policy/v1beta1", component: "k8s", deprecatedIn: "v1.21.0", removedIn: "v1.25.0", replacementApi: "policy/v1", replacementAvailableIn: "v1.21.0", source: "DEPRECATION_SOURCE_HELM_RELEASE", helmRelease: "payments/app", removed: true }], targetVersion: "1.36", scannedAt: "2026-11-16T00:00:00Z", total: 1, removed: 1 }; break;
      case "ListClusterRecommendations": response = { recommendations: [{ namespace: "payments", workloadKind: "Deployment", workloadName: "api", container: "server", replicas: 2, currentRequests: { cpuMillis: "500", memoryBytes: "536870912" }, currentLimits: { memoryBytes: "1073741824" }, recommendedRequests: { cpuMillis: "250", memoryBytes: "268435456" }, recommendedLimits: { memoryBytes: "536870912" }, difference: { cpuMillis: "-250", memoryBytes: "-268435456" }, cpuPercent: -50, memoryPercent: -50, usagePeak: { cpuMillis: "400", memoryBytes: "419430400" }, usageAverage: { cpuMillis: "200", memoryBytes: "209715200" }, source: "RECOMMENDATION_SOURCE_USAGE_ESTIMATE", dataHours: 168, samples: 2880 }], total: 1, summary: { sampledAt: "2026-11-16T00:00:00Z", attemptedAt: "2026-11-16T00:00:00Z", workloads: 1, containers: 1, windowDays: 7, metricsAvailable: true, vpaAvailable: false } }; break;
      case "ListReleases": response = { releases: options.richTables && body.pageToken ? [{ ...stableRelease, name: "release-second", shipped: false, channels: ["RELEASE_CHANNEL_BETA"] }] : [stableRelease], nextPageToken: options.richTables && !body.pageToken ? "release-page-2" : "" }; break;
      case "GetRelease": response = { release: stableRelease }; break;
      case "ListReleaseChannels": response = { channels: [stableChannel] }; break;
      case "GetReleaseChannel": response = { channel: { ...stableChannel, channel: body.channel || stableChannel.channel, history: [{ release: "release-stable", from: "2026-11-16T00:00:00Z", reason: "Promoted" }, { release: "release-old", from: "2026-11-01T00:00:00Z", to: "2026-11-16T00:00:00Z", reason: "Superseded" }] } }; break;
      case "ListNetworkEvents": case "ListClusterEvents": response = { events: options.eventHistory ? body.pageToken ? [{ reason: "Older page event", time: "2026-11-16T08:00:00Z", type: "EVENT_TYPE_NORMAL", message: "Earlier event page: certificate renewal detail. " + "Full controller reconciliation details remain available for inspection. ".repeat(12), objectKind: "Release", objectName: "dns-release", count: 1 }] : [{ reason: "Late day event", time: "2026-11-16T23:59:59.999999999Z", type: "EVENT_TYPE_NORMAL", message: "Reconciled", objectKind: "Cluster", objectName: "payments", count: 1 }, { reason: "Previous day event", time: "2026-11-15T12:00:00Z", type: "EVENT_TYPE_WARNING", message: "Retrying", objectKind: "Cluster", objectName: "payments", count: 1 }] : [], nextPageToken: options.eventHistory && !body.pageToken ? "event-page-2" : "" }; break;
      case "GetAccountOnboarding": response = { onboarding: { accountId: "111122223333", roleName: "cluster-vending-provisioner" } }; break;
      case "BootstrapTenant": response = { account }; break;
      case "VerifyCloudAccount": response = { account }; break;
      case "CreateNetwork": response = { network }; break;
      case "CreateCluster": response = { cluster }; break;
      case "GetFreeze": response = { status: { target: body.target, phase: freezePhase, revision: freezeRevision, platformFrozen: !!options.platformPaused, totalResources: 3, pausedResources: freezePhase === "FREEZE_PHASE_FROZEN" ? 3 : 0 } }; break;
      case "Freeze": freezeRevision = "accepted-freeze"; freezePhase = options.freezeConverges === false ? "FREEZE_PHASE_FREEZING" : "FREEZE_PHASE_FROZEN"; response = { revision: freezeRevision, status: { target: body.target, phase: "FREEZE_PHASE_FREEZING", revision: freezeRevision } }; break;
      case "Unfreeze": freezeRevision = (body.target as { platform?: boolean })?.platform ? "" : "accepted-unfreeze"; freezePhase = "FREEZE_PHASE_UNFROZEN"; response = { revision: freezeRevision, status: { target: body.target, phase: "FREEZE_PHASE_UNFREEZING", revision: freezeRevision } }; break;
      case "UpdateNetwork": {
        if (options.failApply && !body.validateOnly) { status = 400; response = { code: "failed_precondition", message: "The reviewed plan is stale; plan again." }; break; }
        response = { network, updateRecorded: !body.validateOnly, plan: { planId: "reviewed-network-plan", applicable: true, changes: [{ kind: "FlowLog", name: "old-flow-log", action: "PLANNED_ACTION_DELETE", detail: "Dedicated log resources will be removed", retainsCloudResource: false }] } }; break;
      }
      default: status = 501; response = { code: "unimplemented", message: `No fixture for ${method}` };
    }
    if (options.importedNetwork && method === "GetNetwork") {
      response = { network: { ...network, status: { ...network.status, state: "NETWORK_STATE_IMPORTED", import: importPlan } }, asRequested: true };
    }
    if (options.importedCluster && method === "GetCluster") {
      response = { cluster: { ...cluster, status: { ...cluster.status, state: "CLUSTER_STATE_IMPORTED", import: importPlan } }, asRequested: true };
    }
    if (options.richTables && method === "ListClusterDeprecations") {
      const saved = response as { findings: Record<string, unknown>[]; total: number };
      response = { ...saved, findings: [...saved.findings, { ...saved.findings[0], kind: "Ingress", name: "legacy-ingress" }], total: 2, removed: 2 };
    }
    if (options.richTables && method === "ListClusterRecommendations") {
      const saved = response as { recommendations: Record<string, unknown>[]; total: number; summary: Record<string, unknown> };
      response = { ...saved, recommendations: [...saved.recommendations, { ...saved.recommendations[0], workloadKind: "StatefulSet", workloadName: "database", container: "worker" }], total: 2, summary: { ...saved.summary, workloads: 2, containers: 2 } };
    }
    await route.fulfill({ status, contentType: "application/json", body: JSON.stringify(response) });
  });
  return calls;
}

async function mockClipboard(page: Page) {
  await page.addInitScript(() => {
    const state = window as unknown as Window & { copiedValues: string[] };
    state.copiedValues = [];
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText: async (value: string) => { state.copiedValues.push(value); } } });
  });
}

test("YAML creates preserve advanced configuration and require explicit reviewed submission", async ({ page }) => {
  const calls = await mockAPI(page);
  for (const [url, action, kind, spec, method, field] of [
    ["/accounts", "Connect account", "CloudAccount", account.aws ? { aws: account.aws } : {}, "BootstrapTenant", "account"],
    ["/networks", "Create network", "Network", { ...network.spec, flowLogs: { enabled: true, retentionDays: 90 } }, "CreateNetwork", "network"],
    ["/clusters", "Create cluster", "Cluster", { ...cluster.spec, aws: { irsa: false } }, "CreateCluster", "cluster"],
  ] as const) {
    await page.goto(url);
    await page.getByRole("button", { name: action, exact: true }).click();
    await page.getByRole(kind === "Cluster" ? "radio" : "button", { name: "Paste YAML", exact: true }).click();
    const dialog = page.getByRole("dialog");
    const draft = stringify({ apiVersion: "cluster-vending.io/v1", kind, metadata: { name: "yaml-production", tenant: "acme" }, spec });
    const editor = dialog.getByRole("textbox", { name: `${kind} manifest`, exact: true });
    await editor.fill(draft);
    const before = calls.filter(call => call.method === method).length;
    await dialog.getByRole("button", { name: "Review", exact: true }).click();
    await expect(dialog.getByRole("heading", { name: "Review creation", exact: true })).toBeVisible();
    expect(calls.filter(call => call.method === method)).toHaveLength(before);
    await dialog.getByRole("button", { name: "Back", exact: true }).click();
    await expect(editor).toContainText("yaml-production");
    await dialog.getByRole("button", { name: "Review", exact: true }).click();
    await expect(dialog.getByRole("heading", { name: "Review creation", exact: true })).toBeVisible();
    await page.screenshot({ path: `/tmp/cv-ui-${kind.toLowerCase()}-yaml-review.png` });
    await dialog.getByRole("button", { name: action, exact: true }).click();
    await expect(dialog).toHaveCount(0);
    const submitted = calls.filter(call => call.method === method).at(-1)!.body;
    expect(submitted.tenantId).toBe("acme");
    if (field === "account") expect(submitted.account).toMatchObject({ accountName: "yaml-production", aws: account.aws });
    if (field === "network") expect(submitted.network).toMatchObject({ spec: { flowLogs: { enabled: true, retentionDays: 90 } } });
    if (field === "cluster") expect(submitted.cluster).toMatchObject({ spec: { aws: { irsa: false } } });
  }
});

test("YAML import reviews existing infrastructure before recording observe-only discovery", async ({ page }) => {
  const calls = await mockAPI(page);
  for (const [url, label, kind, spec, method] of [
    ["/networks", "network", "NetworkImport", { bootstrapId: accountId, region: "us-west-1", aws: { vpcId: "vpc-0123456789abcdef0", subnetIds: ["subnet-0123456789abcdef0", "subnet-0123456789abcdef1"] } }, "ImportNetwork"],
    ["/clusters", "cluster", "ClusterImport", { networkId, aws: { defaultNodeGroupName: "existing-workers" } }, "ImportCluster"],
  ] as const) {
    await page.goto(url);
    await page.getByRole("button", { name: `Import ${label}`, exact: true }).click();
    await page.getByRole("button", { name: "Paste YAML", exact: true }).click();
    const dialog = page.getByRole("dialog", { name: `Import ${label} from YAML`, exact: true });
    await dialog.getByRole("textbox", { name: `${kind} manifest`, exact: true }).fill(stringify({ apiVersion: "cluster-vending.io/v1", kind, metadata: { name: "existing-prod", tenant: "acme" }, spec }));
    await dialog.getByRole("button", { name: "Review", exact: true }).click();
    await expect(dialog.getByRole("heading", { name: "Review import", exact: true })).toBeVisible();
    await expect(dialog).toContainText("Import stays read-only");
    expect(calls.filter(call => call.method === method)).toHaveLength(0);
    await page.screenshot({ path: `/tmp/cv-ui-${label}-yaml-import.png`, animations: "disabled" });
    await dialog.getByRole("button", { name: `Import ${label}`, exact: true }).click();
    await expect.poll(() => calls.filter(call => call.method === method).length).toBe(1);
    await expect(page.getByRole("alertdialog", { name: `Import ${label} from YAML`, exact: true })).toHaveCount(0);
    await expect(dialog).toHaveCount(0);
    const writes = calls.filter(call => call.method === method);
    expect(writes).toHaveLength(1);
    expect(writes[0].body).toMatchObject({ tenantId: "acme", name: "existing-prod", aws: spec.aws });
    expect(writes[0].body.requestId).toEqual(expect.any(String));
  }
  expect(calls.filter(call => /^TakeOver|^Create/.test(call.method))).toHaveLength(0);
});

test("import YAML rejects another tenant without losing its draft", async ({ page }) => {
  const calls = await mockAPI(page);
  await page.goto("/clusters");
  await page.getByRole("button", { name: "Import cluster", exact: true }).click();
  await page.getByRole("button", { name: "Paste YAML", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Import cluster from YAML", exact: true });
  const editor = dialog.getByRole("textbox", { name: "ClusterImport manifest", exact: true });
  await editor.fill(stringify({ apiVersion: "cluster-vending.io/v1", kind: "ClusterImport", metadata: { name: "existing-prod", tenant: "foreign" }, spec: { networkId, aws: { defaultNodeGroupName: "workers" } } }));
  await dialog.getByRole("button", { name: "Review", exact: true }).click();
  await expect(dialog).toContainText("must match the active tenant");
  await expect(editor).toBeFocused();
  await expect(editor).toContainText("foreign");
  expect(calls.filter(call => call.method === "ImportCluster")).toHaveLength(0);
});

test("tenant selector moves scope out of Settings and drops resource context on switching", async ({ page }) => {
  const calls = await mockAPI(page);
  await page.goto(`/clusters/${clusterId}`);
  await expect(page.getByRole("heading", { name: "payments", exact: true })).toBeVisible();
  await page.getByRole("button", { name: /^Tenant acme/ }).click();
  await page.getByRole("option", { name: "other-tenant", exact: true }).click();
  await expect(page).toHaveURL("/");
  await expect(page.getByRole("button", { name: /^Tenant other-tenant/ })).toBeVisible();
  expect(calls.filter(call => call.method === "GetCluster" && call.body.tenantId === "other-tenant")).toHaveLength(0);
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Settings", exact: true });
  await expect(dialog.getByRole("textbox", { name: /Tenant/ })).toHaveCount(0);
  await expect(dialog.getByLabel("Access token", { exact: false })).toBeVisible();
});

test("tenant creation requires a reviewed name and switches only after API acceptance", async ({ page }) => {
  const calls = await mockAPI(page, { wildcardAdmin: true });
  await page.goto("/");
  await page.getByRole("button", { name: "Create tenant", exact: true }).click();
  const dialog = page.getByRole("dialog");
  const input = dialog.getByRole("textbox", { name: /^Tenant ID/ });
  await input.fill("INVALID_ID");
  await dialog.getByRole("button", { name: "Review tenant", exact: true }).click();
  await expect(dialog).toContainText("Enter a tenant ID");
  await expect(input).toBeFocused();
  await input.fill("team-new");
  await dialog.getByRole("button", { name: "Review tenant", exact: true }).click();
  await expect(dialog.getByRole("heading", { name: "Review tenant", exact: true })).toBeVisible();
  expect(calls.filter(call => call.method === "CreateTenant")).toHaveLength(0);
  await dialog.getByRole("button", { name: "Confirm create tenant", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  await expect(page.getByRole("button", { name: /^Tenant team-new/ })).toBeVisible();
  expect(calls.filter(call => call.method === "CreateTenant").map(call => call.body)).toEqual([{ tenantId: "team-new" }]);
  expect(calls.filter(call => /^(BootstrapTenant|CreateNetwork|CreateCluster)$/.test(call.method))).toHaveLength(0);
  await page.screenshot({ path: "/tmp/cv-ui-tenant-selector.png", animations: "disabled" });
});

test("tenant creation errors preserve the current scope and viewer access remains read-only", async ({ page }) => {
  const calls = await mockAPI(page, { failTenantCreate: true, wildcardAdmin: true });
  await page.goto("/");
  await page.getByRole("button", { name: "Create tenant", exact: true }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByRole("textbox", { name: /^Tenant ID/ }).fill("team-denied");
  await dialog.getByRole("button", { name: "Review tenant", exact: true }).click();
  await dialog.getByRole("button", { name: "Confirm create tenant", exact: true }).click();
  await expect(dialog).toContainText("Tenant creation denied by the API");
  await dialog.getByRole("button", { name: "Back", exact: true }).click();
  await expect(dialog.getByRole("textbox", { name: /^Tenant ID/ })).toHaveValue("team-denied");
  expect(calls.filter(call => call.method === "CreateTenant")).toHaveLength(1);
  await expect(page.getByRole("button", { name: /^Tenant acme/ })).toBeVisible();
  await page.unroute("**/api/rpc/**");
  const viewerCalls = await mockAPI(page, { viewer: true });
  await page.goto("/");
  await expect(page.getByRole("button", { name: /^Tenant acme/ })).toBeVisible();
  await expect(page.getByRole("button", { name: "Create tenant", exact: true })).toHaveCount(0);
  await page.getByRole("button", { name: /^Tenant acme/ }).click();
  await expect(page.getByRole("option", { name: "other-tenant", exact: true })).toHaveCount(0);
  expect(viewerCalls.filter(call => call.method === "CreateTenant")).toHaveLength(0);
});

test("cluster kubeconfig commands fetch current cloud identity and are copyable for viewers", async ({ page }) => {
  const calls = await mockAPI(page, { viewer: true, diagnostics: true });
  await mockClipboard(page);
  await page.goto(`/clusters/${clusterId}`);
  await page.getByRole("button", { name: "Get kubeconfig", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Get kubeconfig", exact: true });
  const command = "aws eks update-kubeconfig --region 'us-west-1' --name 'payments'";
  await expect(dialog).toContainText(command);
  await expect(dialog).not.toContainText("cluster-vending cluster kubeconfig");
  await dialog.getByRole("button", { name: "Copy AWS kubeconfig command", exact: true }).click();
  await expect.poll(() => page.evaluate(() => (window as unknown as { copiedValues: string[] }).copiedValues.at(-1))).toBe(command);
  expect(calls.some(call => call.method === "GetCluster" && call.body.live === true && call.body.clusterId === clusterId && call.body.tenantId === "acme")).toBe(true);
  await page.screenshot({ path: "/tmp/cv-ui-kubeconfig-commands.png", animations: "disabled" });
  expect(calls.filter(call => /^(Create|Update|TakeOver)/.test(call.method))).toHaveLength(0);
});

test("imported clusters require explicit ownership consent before takeover", async ({ page }) => {
  const calls = await mockAPI(page, { importedCluster: true, tenantAdmin: true });
  await page.goto(`/clusters/${clusterId}`);
  await page.getByRole("button", { name: "Take over imported cluster", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Take over imported cluster", exact: true });
  await expect(dialog.getByRole("table", { name: "Resources reviewed for cluster takeover", exact: true })).toContainText("sg-external");
  expect(calls.filter(call => call.method === "TakeOverCluster")).toHaveLength(0);
  await dialog.getByRole("button", { name: "Next", exact: true }).click();
  await dialog.getByRole("textbox", { name: /^Type take over payments/ }).fill("take over payments");
  await expect(dialog.getByRole("button", { name: "Confirm cluster takeover", exact: true })).toBeDisabled();
  await dialog.getByRole("checkbox", { name: /^I confirm every NOT_IMPORTED/ }).check();
  await dialog.getByRole("checkbox", { name: "Skip platform components", exact: true }).check();
  await dialog.getByRole("button", { name: "Confirm cluster takeover", exact: true }).click();
  await expect.poll(() => calls.filter(call => call.method === "TakeOverCluster").length).toBe(1);
  await expect(page.getByRole("alertdialog", { name: "Take over imported cluster", exact: true })).toHaveCount(0);
  await expect(dialog).toHaveCount(0);
  expect(calls.filter(call => call.method === "TakeOverCluster").map(call => call.body)).toEqual([{ tenantId: "acme", clusterId, etag: "cluster-etag", acknowledgeNotImported: true, skipPlatformComponents: true }]);
});

test("network takeover is discoverable and requires ownership acknowledgement", async ({ page }) => {
  const calls = await mockAPI(page, { importedNetwork: true, tenantAdmin: true });
  await page.goto(`/networks/${networkId}`);
  await page.getByRole("button", { name: "Take over imported network", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Take over imported network", exact: true });
  await dialog.getByRole("textbox", { name: /^Type take over prod-network/ }).fill("take over prod-network");
  await expect(dialog.getByRole("button", { name: "Confirm network takeover", exact: true })).toBeDisabled();
  await dialog.getByRole("checkbox", { name: /^I confirm every NOT_IMPORTED/ }).check();
  await dialog.getByRole("button", { name: "Confirm network takeover", exact: true }).click();
  await expect.poll(() => calls.filter(call => call.method === "TakeOverNetwork").length).toBe(1);
  await expect(page.getByRole("alertdialog", { name: "Take over imported network", exact: true })).toHaveCount(0);
  await expect(dialog).toHaveCount(0);
  expect(calls.filter(call => call.method === "TakeOverNetwork").map(call => call.body)).toEqual([{ tenantId: "acme", networkId, etag: "network-etag", acknowledgeNotImported: true }]);
});

test("YAML validation rejects another tenant and retains the pasted draft", async ({ page }) => {
  const calls = await mockAPI(page);
  await page.goto("/accounts");
  await page.getByRole("button", { name: "Connect account", exact: true }).click();
  await page.getByRole("button", { name: "Paste YAML", exact: true }).click();
  const dialog = page.getByRole("dialog");
  const editor = dialog.getByRole("textbox", { name: "CloudAccount manifest", exact: true });
  await editor.fill(stringify({ apiVersion: "cluster-vending.io/v1", kind: "CloudAccount", metadata: { name: "production", tenant: "other" }, spec: { aws: account.aws } }));
  await dialog.getByRole("button", { name: "Review", exact: true }).click();
  await expect(dialog.getByText(/must match the active tenant/)).toBeVisible();
  await expect(editor).toBeFocused();
  await expect(editor).toContainText("other");
  expect(calls.filter(call => call.method === "BootstrapTenant")).toHaveLength(0);
  await dialog.getByRole("button", { name: "Use guided form", exact: true }).click();
  await expect(page.getByRole("textbox", { name: "Display name" })).toBeVisible();
  await page.getByRole("button", { name: "Paste YAML", exact: true }).click();
  await expect(editor).toContainText("other");
});

test("flat account, network and cluster tables search complete inventories", async ({ page }) => {
  const calls = await mockAPI(page, { pagedInventory: true });
  for (const [url, search, term, tableName, match] of [
    ["/accounts", "Search accounts", "111122223333", "Cloud accounts", "Production"],
    ["/networks", "Search networks", "second-network", "Tenant networks", "Second page network"],
    ["/clusters", "Search clusters", "second-cluster", "Clusters", "second-cluster"],
  ]) {
    await page.goto(url);
    const input = page.getByRole("searchbox", { name: search, exact: true });
    await input.fill(term);
    const table = page.getByRole("table", { name: tableName, exact: true });
    await expect(table.getByRole("row")).toHaveCount(2);
    await expect(table).toContainText(match);
    await input.fill("no-resource-matches");
    await expect(table).toHaveCount(0);
    await input.fill("");
  }
  expect(calls.some(call => call.method === "ListNetworks" && call.body.pageToken === "page-2")).toBe(true);
  expect(calls.some(call => call.method === "ListClusters" && call.body.pageToken === "page-2")).toBe(true);
});

test("list selectors filter independently of search and can be cleared", async ({ page }) => {
  await mockAPI(page);
  await page.goto("/accounts");
  await page.getByRole("combobox", { name: "Access state", exact: true }).click();
  await page.getByRole("option", { name: "Not verified", exact: true }).click();
  await expect(page.getByText("No accounts match the current filters.")).toBeVisible();
  await page.getByRole("button", { name: "Clear account filters", exact: true }).click();
  await expect(page.getByRole("table", { name: "Cloud accounts" })).toContainText("Production");
  await page.goto("/clusters");
  await page.getByRole("combobox", { name: "Channel", exact: true }).click();
  await page.getByRole("option", { name: "Alpha", exact: true }).click();
  await expect(page.getByText("No clusters match these filters.")).toBeVisible();
  await page.getByRole("button", { name: "Clear filters", exact: true }).click();
  await expect(page.getByRole("table", { name: "Clusters", exact: true })).toContainText("payments");
});

test("detail resource search and pause filters compose and reset", async ({ page }) => {
  await mockAPI(page);
  for (const [url, cloudId, pausedLabel, activeOption] of [
    [`/accounts/${accountId}`, "production-provider", "Paused resources", "Paused only"],
    [`/networks/${networkId}`, "vpc-0123456789abcdef0", "Pause status", "Paused"],
    [`/clusters/${clusterId}`, "payments-workers-external", "Paused", "Active"],
  ]) {
    await page.goto(url);
    await page.getByRole("tab", { name: "Resources", exact: true }).click();
    const search = page.getByRole("searchbox", { name: "Search resources", exact: true });
    await search.fill(cloudId);
    await expect(page.getByRole("table")).toContainText(cloudId);
    await page.getByRole("combobox", { name: pausedLabel, exact: true }).click();
    await page.getByRole("option", { name: activeOption, exact: true }).click();
    await expect(page.getByRole("table")).toHaveCount(0);
    await page.getByRole("button", { name: "Clear resource filters", exact: true }).click();
    await expect(search).toHaveValue("");
    await expect(page.getByRole("table")).toContainText(cloudId);
  }
});

test("legacy network cluster names do not create a duplicate UUID row", async ({ page }) => {
  await mockAPI(page, { legacyNetworkNames: true });
  await page.goto(`/networks/${networkId}`);
  const table = page.getByRole("table", { name: "Clusters belonging to this network" });
  await expect(table.getByRole("link", { name: "payments", exact: true })).toBeVisible();
  await expect(table.getByRole("row")).toHaveCount(2);
  await expect(table).toContainText("1.35");
});

test("idle pages refresh manually until Live updates is explicitly enabled", async ({ page }) => {
  await page.clock.install();
  const calls = await mockAPI(page);
  await page.goto(`/networks/${networkId}`);
  await expect(page.getByRole("heading", { level: 1 })).toHaveText("Production network");
  await expect(page.getByRole("table", { name: "Clusters belonging to this network" })).toContainText("payments");
  const initial = calls.filter(call => call.method === "GetNetwork").length;
  await page.clock.runFor(16_000);
  expect(calls.filter(call => call.method === "GetNetwork")).toHaveLength(initial);
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  const toggle = page.getByRole("checkbox", { name: /^Live updates\b/ });
  await expect(toggle).not.toBeChecked();
  await toggle.check();
  await page.getByRole("dialog", { name: "Settings" }).getByRole("button", { name: "Close", exact: true }).last().click();
  await expect.poll(() => calls.filter(call => call.method === "GetNetwork").length).toBeGreaterThan(initial);
  const enabled = calls.filter(call => call.method === "GetNetwork").length;
  await page.clock.runFor(6_000);
  await expect.poll(() => calls.filter(call => call.method === "GetNetwork").length).toBeGreaterThan(enabled);
});

test("Settings uses aligned full-width fields and fits desktop and mobile", async ({ page }) => {
  await mockAPI(page);
  await page.goto("/accounts");
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Settings", exact: true });
  await expect(dialog).toBeVisible();
  await expect(dialog.getByRole("textbox", { name: /Tenant/ })).toHaveCount(0);
  const token = dialog.getByLabel("Access token", { exact: false });
  const tokenBounds = await token.boundingBox();
  expect(tokenBounds!.width).toBeGreaterThan(300);
  await expect(dialog.getByRole("checkbox", { name: "Live updates", exact: true })).toBeVisible();
  await page.screenshot({ path: "/tmp/cv-ui-settings-desktop.png", animations: "disabled" });
  await page.keyboard.press("Escape");
  await expect(dialog).toHaveCount(0);
  await page.setViewportSize({ width: 390, height: 844 });
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  await expect(dialog).toBeVisible();
  const bounds = await dialog.boundingBox();
  expect(bounds!.x).toBeGreaterThanOrEqual(0);
  expect(bounds!.x + bounds!.width).toBeLessThanOrEqual(390);
  expect(bounds!.height).toBeLessThanOrEqual(844);
  await dialog.getByRole("button", { name: "Connect", exact: true }).scrollIntoViewIfNeeded();
  await expect(dialog.getByRole("button", { name: "Connect", exact: true })).toBeVisible();
  await page.screenshot({ path: "/tmp/cv-ui-settings-mobile.png", animations: "disabled" });
});

test("detail activity and channel history have no date filters", async ({ page }) => {
  await mockAPI(page, { eventHistory: true });
  for (const [url, name] of [[`/networks/${networkId}`, "Network events"], [`/clusters/${clusterId}`, "Cluster events"]]) {
    await page.goto(url);
    await page.getByRole("tab", { name: "Activity", exact: true }).click();
    const table = page.getByRole("table", { name, exact: true });
    await expect(table.getByRole("columnheader")).toHaveText(["Time (UTC)", "Type", "Reason", "Message", "Kind", "Resource", "Count"]);
    await expect(page.locator('input[type="date"], input[type="datetime-local"]')).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Refresh activity", exact: true })).toBeVisible();
    const heading = await page.getByRole("heading", { name: "Activity", exact: true }).boundingBox();
    const frame = await table.boundingBox();
    expect(Math.abs(heading!.x - frame!.x)).toBeLessThan(2);
  }
  await page.goto("/channels/beta");
  await page.getByRole("tab", { name: "History", exact: true }).click();
  await expect(page.getByRole("table", { name: "Channel release history", exact: true })).toContainText("release-old");
  await expect(page.getByRole("combobox", { name: "Date field", exact: true })).toHaveCount(0);
  await expect(page.locator('input[type="date"], input[type="datetime-local"]')).toHaveCount(0);
  await page.screenshot({ path: "/tmp/cv-ui-beta-history.png", fullPage: true, animations: "disabled" });
});

test("tenant viewers can inspect and copy YAML for accounts, networks and clusters", async ({ page }) => {
  const calls = await mockAPI(page, { viewer: true });
  await mockClipboard(page);
  for (const [url, kind, id] of [[`/accounts/${accountId}`, "CloudAccount", accountId], [`/networks/${networkId}`, "Network", networkId], [`/clusters/${clusterId}`, "Cluster", clusterId]]) {
    await page.goto(url);
    await page.getByRole("button", { name: "View YAML", exact: true }).click();
    const dialog = page.getByRole("dialog", { name: `${kind} YAML`, exact: true });
    await expect(dialog).toBeVisible();
    const editor = dialog.getByRole("textbox", { name: `${kind} YAML configuration`, exact: true });
    await expect(editor).toContainText(`kind: ${kind}`);
    await expect(editor).toContainText(id);
    await expect(editor).toHaveAttribute("aria-readonly", "true");
    await dialog.getByRole("button", { name: `Copy ${kind} YAML configuration`, exact: true }).click();
    await expect.poll(() => page.evaluate(() => (window as unknown as { copiedValues: string[] }).copiedValues.at(-1) ?? "")).toContain(`kind: ${kind}`);
    await page.screenshot({ path: `/tmp/cv-ui-${kind.toLowerCase()}-yaml.png`, animations: "disabled" });
    await dialog.getByRole("button", { name: "Close YAML", exact: true }).click();
    await expect(dialog).toHaveCount(0);
  }
  expect(calls.some(call => call.method === "GetNetwork" && call.body.asRequested === true)).toBeTruthy();
  expect(calls.some(call => call.method === "GetCluster" && call.body.asRequested === true)).toBeTruthy();
  expect(calls.filter(call => /^(Create|Update|Delete|Bootstrap|Freeze|Unfreeze)/.test(call.method))).toHaveLength(0);
});

test("shared tables support pointer and keyboard resizing without configuration menus", async ({ page }) => {
  await mockAPI(page);
  await page.goto("/");
  const table = page.getByRole("table", { name: "Tenant resource inventory", exact: true });
  const name = table.getByRole("columnheader", { name: "Resource", exact: true });
  const initial = (await name.boundingBox())!.width;
  const handle = name.getByRole("separator");
  await handle.focus();
  await handle.press("Shift+ArrowRight");
  await expect.poll(async () => (await name.boundingBox())!.width).toBeGreaterThan(initial + 20);
  await page.getByRole("button", { name: "Reset Tenant resource inventory column widths", exact: true }).click();
  await expect.poll(async () => (await name.boundingBox())!.width).toBeLessThan(initial + 2);
  const bounds = await handle.boundingBox();
  await page.mouse.move(bounds!.x + bounds!.width / 2, bounds!.y + 8);
  await page.mouse.down();
  await page.mouse.move(bounds!.x + bounds!.width / 2 + 40, bounds!.y + 8, { steps: 6 });
  await page.mouse.up();
  await expect.poll(async () => (await name.boundingBox())!.width).toBeGreaterThan(initial + 15);
  await expect(page.getByRole("button", { name: /^Columns for/ })).toHaveCount(0);
  await expect(page.getByRole("spinbutton", { name: /column width/ })).toHaveCount(0);
  await page.getByRole("button", { name: "Reset Tenant resource inventory column widths", exact: true }).click();
  await expect.poll(async () => (await name.boundingBox())!.width).toBeLessThan(initial + 2);
  await page.screenshot({ path: "/tmp/cv-ui-table-resize.png", animations: "disabled" });
});

test("Kind filters compose with search across overview and all resource inventories", async ({ page }) => {
  await mockAPI(page, { richTables: true });
  await page.goto("/");
  const overview = page.getByRole("table", { name: "Tenant resource inventory", exact: true });
  await page.getByRole("combobox", { name: "Resource kind", exact: true }).click();
  await page.getByRole("option", { name: "Cluster", exact: true }).click();
  await expect(overview.getByRole("row")).toHaveCount(4);
  await expect(overview).toContainText("Production network");
  await expect(overview).toContainText("Context");
  await expect(overview).toContainText("payments");
  await page.getByRole("button", { name: "Clear resource filters", exact: true }).click();
  await expect(overview.getByRole("row")).toHaveCount(4);
  for (const [url, kind, resource, tableName] of [
    [`/accounts/${accountId}`, "Secret", "account-credentials", "Account managed resource inventory"],
    [`/networks/${networkId}`, "Subnet", "private-subnet", "Network managed resource inventory"],
    [`/clusters/${clusterId}`, "Release", "dns-release", "Cluster cloud resources"],
  ]) {
    await page.goto(url);
    await page.getByRole("tab", { name: "Resources", exact: true }).click();
    await (url.startsWith("/clusters/") ? page.getByRole("button", { name: /^Resource kind / }) : page.getByRole("combobox", { name: "Resource kind", exact: true })).click();
    await page.getByRole("option", { name: kind, exact: true }).click();
    const inventory = tableName === "Cluster cloud resources" ? page.getByRole("table").first() : page.getByRole("table", { name: tableName, exact: true });
    await expect(inventory.getByRole("row")).toHaveCount(2);
    await expect(inventory).toContainText(resource);
    await page.getByRole("searchbox", { name: "Search resources", exact: true }).fill("nothing-matches");
    await expect(page.getByRole("table")).toHaveCount(0);
    await page.getByRole("button", { name: "Clear resource filters", exact: true }).click();
    await expect(page.getByRole("table").first().getByRole("row")).toHaveCount(3);
  }
});

test("event Kind and message search include later pages and full messages appear on hover", async ({ page }) => {
  const calls = await mockAPI(page, { eventHistory: true, viewer: true });
  for (const [url, tableName] of [[`/networks/${networkId}`, "Network events"], [`/clusters/${clusterId}`, "Cluster events"]]) {
    await page.goto(url);
    await page.getByRole("tab", { name: "Activity", exact: true }).click();
    await page.getByRole("combobox", { name: "Event kind", exact: true }).click();
    await page.getByRole("option", { name: "Release", exact: true }).click();
    const table = page.getByRole("table", { name: tableName, exact: true });
    await expect(table).toContainText("Older page event");
    await expect(table.getByRole("row")).toHaveCount(2);
    const search = page.getByRole("searchbox", { name: "Search events", exact: true });
    await search.fill("certificate renewal detail");
    await expect(table).toContainText("Older page event");
    await expect(page.getByRole("button", { name: "View full message", exact: true })).toHaveCount(0);
    const message = table.getByText(/^Earlier event page: certificate renewal detail/).first();
    await message.hover();
    await expect(page.getByRole("tooltip")).toContainText("Full controller reconciliation details");
    const tooltipText = page.getByRole("tooltip").getByText(/^Earlier event page:/).first();
    const colors = await tooltipText.evaluate(element => [getComputedStyle(element).color, getComputedStyle(element.closest('[role="tooltip"]')!).color]);
    expect(colors[0]).toBe(colors[1]);
    await page.screenshot({ path: `/tmp/cv-ui-${tableName.startsWith("Network") ? "network" : "cluster"}-event-tooltip.png`, animations: "disabled" });
    await page.keyboard.press("Escape");
    await expect(page.getByRole("tooltip")).toHaveCount(0);
    await page.mouse.move(0, 0);
    await message.focus();
    await expect(page.getByRole("tooltip")).toContainText("Full controller reconciliation details");
    await page.keyboard.press("Escape");
    await expect(page.getByRole("tooltip")).toHaveCount(0);
    await page.getByRole("checkbox", { name: "Warnings only", exact: true }).check();
    await expect(table).toHaveCount(0);
    await page.getByRole("button", { name: "Clear activity filters", exact: true }).click();
    await expect(search).toHaveValue("");
    await expect(page.getByRole("table", { name: tableName, exact: true })).toContainText("Previous day event");
  }
  expect(calls.some(call => call.method === "ListClusterEvents" && call.body.pageToken === "event-page-2")).toBe(true);
  expect(calls.some(call => call.method === "ListNetworkEvents" && call.body.pageToken === "event-page-2")).toBe(true);
});

test("detail table search keeps reset accessible when no rows match", async ({ page }) => {
  await mockAPI(page, { diagnostics: true });
  await page.goto(`/clusters/${clusterId}`);
  const search = page.getByRole("searchbox", { name: "Search Cluster health conditions", exact: true });
  await search.fill("not-an-observation");
  await expect(page.getByRole("table", { name: "Cluster health conditions", exact: true })).toContainText("No rows match");
  await page.getByRole("button", { name: "Clear Cluster health conditions search", exact: true }).click();
  await expect(page.getByRole("table", { name: "Cluster health conditions", exact: true })).toContainText("Reconciled");
});

test("release pagination and pure component catalog filters retain complete inventories", async ({ page }) => {
  const calls = await mockAPI(page, { richTables: true, componentCatalog: true });
  await page.goto("/releases");
  await page.getByRole("searchbox", { name: "Search releases", exact: true }).fill("release-second");
  const releases = page.getByRole("table", { name: "Releases", exact: true });
  await expect(releases).toContainText("release-second");
  await expect(releases).not.toContainText("release-stable");
  await page.getByRole("combobox", { name: "Release channel", exact: true }).click();
  await page.getByRole("option", { name: "Beta", exact: true }).click();
  await expect(releases.getByRole("row")).toHaveCount(2);
  await page.getByRole("button", { name: "Clear release filters", exact: true }).click();
  await expect(releases.getByRole("row")).toHaveCount(3);
  await page.goto(`/components?cluster=${clusterId}`);
  await page.getByRole("searchbox", { name: "Search component catalog", exact: true }).fill("policies");
  const catalog = page.getByRole("table", { name: "Component catalog", exact: true });
  await expect(catalog).toContainText("policies");
  await expect(catalog).not.toContainText("external-dns");
  await page.getByRole("button", { name: "Clear catalog filters", exact: true }).click();
  await expect(catalog.getByRole("row")).toHaveCount(4);
  expect(calls.some(call => call.method === "ListReleases" && call.body.pageToken === "release-page-2")).toBe(true);
  expect(calls.filter(call => call.method === "ListComponentDefinitions").every(call => !call.body.clusterId)).toBe(true);
  expect(calls.some(call => call.method === "ListComponents")).toBe(false);
});

test("release inventory distinguishes current channel offerings from other valid releases", async ({ page }) => {
  await mockAPI(page, { richTables: true });
  await page.goto("/releases");
  const releases = page.getByRole("table", { name: "Releases", exact: true });
  await expect(releases.getByRole("row").filter({ hasText: "release-stable" })).toContainText("Active · Current offering");
  await expect(releases.getByRole("row").filter({ hasText: "release-second" })).toContainText("Available");
  await expect(releases).not.toContainText("Invalid configuration");
  await page.screenshot({ path: "/tmp/cv-ui-release-offerings.png", fullPage: true });
  await page.getByRole("combobox", { name: "Release state", exact: true }).click();
  await page.getByRole("option", { name: "Active · Current offering", exact: true }).click();
  await expect(releases.getByRole("row")).toHaveCount(2);
  await expect(releases).not.toContainText("release-second");
});

test("Kind filters narrow deprecation findings and workload recommendations", async ({ page }) => {
  await mockAPI(page, { richTables: true });
  await page.goto(`/clusters/${clusterId}`);
  await page.getByRole("tab", { name: "Deprecated APIs", exact: true }).click();
  await page.getByRole("combobox", { name: "Kind", exact: true }).click();
  await page.getByRole("option", { name: "Ingress", exact: true }).click();
  const findings = page.getByRole("table", { name: "Cluster deprecated API findings", exact: true });
  await expect(findings.getByRole("row")).toHaveCount(2);
  await expect(findings).toContainText("legacy-ingress");
  await expect(findings).not.toContainText("payments-budget");
  await page.getByRole("button", { name: "Clear finding filters", exact: true }).click();
  await expect(findings.getByRole("row")).toHaveCount(3);
  await page.getByRole("tab", { name: "Recommendations", exact: true }).click();
  await page.getByRole("combobox", { name: "Kind", exact: true }).click();
  await page.getByRole("option", { name: "StatefulSet", exact: true }).click();
  const recommendations = page.getByRole("table", { name: "Container resource recommendations", exact: true });
  await expect(recommendations.getByRole("row")).toHaveCount(2);
  await expect(recommendations).toContainText("database");
  await expect(recommendations).not.toContainText("Deployment");
  await page.getByRole("button", { name: "Clear recommendation filters", exact: true }).click();
  await expect(recommendations.getByRole("row")).toHaveCount(3);
});

test("cluster diagnostics are always-visible tables with copyable identifiers", async ({ page }) => {
  await mockAPI(page, { diagnostics: true });
  await page.goto(`/clusters/${clusterId}`);
  const health = page.getByRole("table", { name: "Cluster health conditions", exact: true });
  await expect(health).toContainText("Reconciled");
  await expect(health).toContainText("All resources are current");
  const identifiers = page.getByRole("table", { name: "Provider identifiers and endpoint", exact: true });
  await expect(identifiers).toContainText("https://example.eks.amazonaws.com");
  await expect(identifiers).toContainText("arn:aws:eks:us-west-1:111122223333:cluster/payments");
  await expect(page.getByRole("button", { name: "All health conditions", exact: true })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Provider identifiers and endpoint", exact: true })).toHaveCount(0);
  await expect(identifiers.getByRole("button", { name: "Copy Cluster ARN", exact: true })).toHaveCount(1);
  await identifiers.scrollIntoViewIfNeeded();
  await page.screenshot({ path: "/tmp/cv-ui-cluster-diagnostics.png", fullPage: true, animations: "disabled" });
});

test("cluster insights load tenant-scoped saved findings and recommendations on demand", async ({ page }) => {
  const calls = await mockAPI(page);
  await page.goto(`/clusters/${clusterId}`);
  await expect(page.getByRole("button", { name: "View deprecated APIs", exact: true })).toBeVisible();
  expect(calls.filter(call => call.method === "ListClusterDeprecations" || call.method === "ListClusterRecommendations")).toHaveLength(0);
  await page.getByRole("tab", { name: "Deprecated APIs", exact: true }).click();
  const findings = page.getByRole("table", { name: "Cluster deprecated API findings", exact: true });
  await expect(findings).toContainText("payments-budget");
  await expect(findings).toContainText("policy/v1beta1");
  await expect(findings).toContainText("policy/v1");
  await expect(findings).toContainText("Removed");
  await page.getByRole("searchbox", { name: "Search findings", exact: true }).fill("missing-resource");
  await expect(findings).toHaveCount(0);
  await page.getByRole("button", { name: "Clear finding filters", exact: true }).click();
  await expect(findings).toContainText("payments-budget");
  await page.screenshot({ path: "/tmp/cv-ui-cluster-deprecated-apis.png", fullPage: true, animations: "disabled" });
  await page.getByRole("tab", { name: "Recommendations", exact: true }).click();
  const recommendations = page.getByRole("table", { name: "Container resource recommendations", exact: true });
  await expect(recommendations).toContainText("500 mCPU");
  await expect(recommendations).toContainText("250 mCPU");
  await expect(recommendations).toContainText("512 MiB");
  await expect(recommendations).toContainText("256 MiB");
  await expect(recommendations).toContainText("Usage estimate");
  await recommendations.getByRole("button", { name: "server", exact: true }).click();
  await expect(page.getByRole("dialog")).toBeVisible();
  await expect(page.getByRole("dialog")).toContainText(/2,?880/);
  await page.keyboard.press("Escape");
  await page.getByRole("checkbox", { name: /Only changes/ }).check();
  await expect.poll(() => calls.filter(call => call.method === "ListClusterRecommendations").at(-1)?.body.onlyChanges).toBe(true);
  for (const call of calls.filter(call => call.method === "ListClusterDeprecations" || call.method === "ListClusterRecommendations")) expect(call.body).toMatchObject({ tenantId: "acme", clusterId });
  await page.screenshot({ path: "/tmp/cv-ui-cluster-recommendations.png", fullPage: true, animations: "disabled" });
});

test("a cluster that has never been scanned is not shown as compatible", async ({ page }) => {
  await mockAPI(page, { neverScanned: true, viewer: true });
  await page.goto(`/clusters/${clusterId}`);
  await page.getByRole("tab", { name: "Deprecated APIs", exact: true }).click();
  await expect(page.getByText("Never scanned", { exact: true }).first()).toBeVisible();
  await expect(page.getByText("No deprecated API findings", { exact: true })).toHaveCount(0);
});

test("cluster tenant catalog enables configured components only after exact reviewed confirmation", async ({ page }) => {
  const calls = await mockAPI(page, { componentCatalog: true });
  await page.goto(`/clusters/${clusterId}`);
  await page.getByRole("tab", { name: "Components", exact: true }).click();
  const table = page.getByRole("table", { name: "Cluster tenant component catalog", exact: true });
  const blocked = table.getByRole("row").filter({ has: page.getByRole("link", { name: "blocked-component", exact: true }) });
  await expect(blocked).toContainText("Disabled by platform policy");
  await expect(blocked.getByRole("button", { name: "Enable component", exact: true })).toHaveAttribute("aria-disabled", "true");
  const row = table.getByRole("row").filter({ has: page.getByRole("link", { name: "policies", exact: true }) });
  await expect(row).toContainText("Disabled");
  await row.getByRole("button", { name: "Enable component", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Enable policies", exact: true });
  await dialog.getByRole("textbox", { name: "Tenant values (YAML object)", exact: true }).fill("replicaCount: 3\n");
  await dialog.getByRole("button", { name: "Validate and review", exact: true }).click();
  await expect(dialog.getByRole("heading", { name: "Review enable", exact: true })).toBeVisible();
  const preview = calls.find(call => call.method === "EnableComponent")!.body;
  expect(preview.validateOnly).toBe(true);
  expect(calls.filter(call => call.method === "EnableComponent" && !call.body.validateOnly)).toHaveLength(0);
  await dialog.getByRole("button", { name: "Apply reviewed enable", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  const applied = calls.find(call => call.method === "EnableComponent" && !call.body.validateOnly)!.body;
  expect(applied.requestId).toBe(preview.requestId);
  expect(applied.component).toEqual(preview.component);
  expect(applied.component).toMatchObject({ clusterId, component: "policies", values: { replicaCount: 3 } });
  expect(applied.tenantId).toBe("acme");
  await expect(row.getByRole("button", { name: "Manage component", exact: true })).toBeVisible();
  await page.screenshot({ path: "/tmp/cv-ui-cluster-tenant-components.png", fullPage: true, animations: "disabled" });
});

test("cluster component catalog is read-only for tenant viewers", async ({ page }) => {
  await mockAPI(page, { componentCatalog: true, viewer: true });
  await page.goto(`/clusters/${clusterId}`);
  await page.getByRole("tab", { name: "Components", exact: true }).click();
  const table = page.getByRole("table", { name: "Cluster tenant component catalog", exact: true });
  await expect(table).toContainText("policies");
  await expect(table.getByRole("button", { name: "Enable component", exact: true })).toHaveCount(0);
  await expect(table.getByRole("button", { name: "Manage component", exact: true })).toHaveCount(0);
});

test("account IDs remain visible and each copy control copies its adjacent identity once", async ({ page }) => {
  await mockClipboard(page);
  await mockAPI(page);
  await page.goto("/accounts");
  const table = page.getByRole("table", { name: "Cloud accounts" });
  await expect(table).toContainText(accountId);
  await expect(table).toContainText("111122223333");
  const identity = table.getByText(accountId, { exact: true });
  const bounds = await identity.boundingBox();
  expect(bounds?.width).toBeGreaterThan(40);
  await table.hover();
  await table.getByRole("button", { name: "Copy Platform account ID", exact: true }).click();
  await expect.poll(() => page.evaluate(() => (window as unknown as Window & { copiedValues: string[] }).copiedValues)).toEqual([accountId]);
  await page.screenshot({ path: "/tmp/cv-ui-accounts-table.png", fullPage: true });
});

test("cluster tables show status, Kubernetes, network, channel and inherited tier", async ({ page }) => {
  await mockAPI(page);
  await page.goto("/clusters");
  const table = page.getByRole("table", { name: "Clusters", exact: true });
  for (const name of ["ID", "Status", "Kubernetes", "Network", "Region", "Channel", "Tier"]) await expect(table.getByRole("columnheader", { name, exact: true })).toBeVisible();
  for (const value of ["payments", "Running", "1.35", "Production network", "Stable", "Tier 1"]) await expect(table).toContainText(value);
  await page.screenshot({ path: "/tmp/cv-ui-clusters-table.png", fullPage: true });
});

test("table statuses preserve the full ink height of deleting and missing labels", async ({ page }) => {
  await mockAPI(page);
  await page.route("**/api/rpc/cloud.v1.ClusterService/ListClusters", route => route.fulfill({ contentType: "application/json", body: JSON.stringify({ clusters: [{ ...cluster, status: { ...cluster.status, state: "CLUSTER_STATE_DELETING" } }] }) }));
  await page.route("**/api/rpc/cloud.v1.NetworkService/GetNetwork", route => route.fulfill({ contentType: "application/json", body: JSON.stringify({ network: { ...network, status: { ...network.status, managedResources: [{ ...network.status.managedResources[0], state: "MANAGED_RESOURCE_STATE_MISSING", ready: "CONDITION_STATUS_FALSE", synced: "CONDITION_STATUS_FALSE" }] } }, asRequested: true }) }));
  for (const [url, label, tableName] of [["/clusters", "Deleting", "Clusters"], [`/networks/${networkId}`, "Missing", "Network managed resource inventory"]]) {
    await page.goto(url);
    if (label === "Missing") await page.getByRole("tab", { name: "Resources", exact: true }).click();
    const text = page.getByRole("table", { name: tableName, exact: true }).getByText(label, { exact: true });
    await expect(text).toBeVisible();
    await page.evaluate(() => document.fonts.ready);
    const metrics = await text.evaluate(element => {
      const context = document.createElement("canvas").getContext("2d")!;
      context.font = getComputedStyle(element).font;
      const ink = context.measureText(element.textContent!);
      return { available: element.getBoundingClientRect().height, ink: ink.actualBoundingBoxAscent + ink.actualBoundingBoxDescent };
    });
    expect(metrics.available).toBeGreaterThanOrEqual(metrics.ink);
    await page.screenshot({ path: `/tmp/cv-ui-${label.toLowerCase()}-descenders.png`, fullPage: true });
  }
});

test("resource and operator detail headers use the same page gutter", async ({ page }) => {
  await mockAPI(page);
  for (const [url, name] of [[`/accounts/${accountId}`, "Production"], [`/networks/${networkId}`, "Production network"], [`/clusters/${clusterId}`, "payments"], ["/releases/release-stable", "release-stable"], ["/channels/stable", "Stable"], ["/freeze", "Freeze controls"], [`/components/external-dns?cluster=${clusterId}`, "external-dns"]]) {
    await page.goto(url);
    const heading = page.getByRole("heading", { level: 1 });
    await expect(heading).toHaveText(name);
    const title = await heading.boundingBox();
    const breadcrumb = await page.getByRole("navigation", { name: "Breadcrumb" }).boundingBox();
    expect(Math.abs(title!.x - breadcrumb!.x), url).toBeLessThan(2);
  }
});

test("channel versions use its current offered release and exact Kubernetes minor", async ({ page }) => {
  await mockClipboard(page);
  const calls = await mockAPI(page);
  await page.goto("/channels/stable");
  await expect(page.getByRole("heading", { name: "AWS EKS add-on versions", exact: true })).toHaveCount(0);
  await page.getByRole("tab", { name: "Components", exact: true }).click();
  await expect(page.getByRole("heading", { name: "AWS EKS add-on versions", exact: true })).toHaveCount(0);
  const table = page.getByRole("table", { name: "Release component pins by Kubernetes minor" });
  await expect(table).toContainText("0.16.0");
  await expect(table).not.toContainText("0.17.0");
  expect([...new Set(calls.filter(call => call.method === "GetRelease").map(call => call.body.name))]).toEqual(["release-stable"]);
  await table.getByRole("button", { name: "Inspect locked values", exact: true }).click();
  const values = page.getByRole("textbox", { name: "Locked Helm values for external-dns on Kubernetes 1.35", exact: true });
  await expect(values).toContainText("policy: sync");
  await expect(values).toHaveAttribute("aria-readonly", "true");
  await page.getByRole("button", { name: "Copy Locked Helm values for external-dns on Kubernetes 1.35", exact: true }).click();
  await expect.poll(() => page.evaluate(() => (window as unknown as Window & { copiedValues: string[] }).copiedValues.at(-1))).toContain("policy: sync");
  await page.screenshot({ path: "/tmp/cv-ui-channel-versions.png", fullPage: true });
});

test("channel Clusters tab lists subscriptions across all tenant pages, including Stable defaults", async ({ page }) => {
  const calls = await mockAPI(page, { channelSubscriptions: true, tenantAdmin: true });
  for (const [channel, included, excluded] of [
    ["alpha", ["alpha-test"], ["payments", "beta-test", "automatic-stable", "unassigned-manual"]],
    ["beta", ["beta-test"], ["payments", "alpha-test", "automatic-stable", "unassigned-manual"]],
    ["stable", ["payments", "automatic-stable"], ["alpha-test", "beta-test", "unassigned-manual"]],
  ] as const) {
    await page.goto(`/channels/${channel}`);
    await page.getByRole("tab", { name: "Clusters", exact: true }).click();
    const table = page.getByRole("table", { name: "Channel subscribed clusters", exact: true });
    for (const name of included) await expect(table.getByRole("link", { name, exact: true })).toBeVisible();
    for (const name of excluded) await expect(table.getByRole("link", { name, exact: true })).toHaveCount(0);
    await expect(table).toContainText("Production network");
    await expect(table).toContainText("1.35");
    await expect(table).toContainText("Tier 1");
    if (channel === "stable") {
      const input = page.getByRole("searchbox", { name: "Search subscribed clusters", exact: true });
      await input.fill("automatic-stable");
      await expect(table.getByRole("row")).toHaveCount(2);
      await expect(table).toContainText("Automatic");
      await input.fill("no-subscriber-match");
      await expect(table).toHaveCount(0);
      await input.fill("");
      await expect(table.getByRole("row")).toHaveCount(3);
      await page.screenshot({ path: "/tmp/cv-ui-channel-subscribers.png", fullPage: true, animations: "disabled" });
    }
  }
  expect(calls.some(call => call.method === "ListClusters" && call.body.pageToken === "subscriptions-2")).toBe(true);
  expect(calls.filter(call => call.method === "ListClusters").every(call => call.body.tenantId === "acme")).toBe(true);
});

test("cluster component values validate before applying exact reviewed settings", async ({ page }) => {
  const calls = await mockAPI(page);
  await page.goto(`/clusters/${clusterId}`);
  await page.getByRole("tab", { name: "Components", exact: true }).click();
  const row = page.getByRole("table", { name: "Cluster tenant component catalog", exact: true }).getByRole("row").filter({ has: page.getByRole("link", { name: "external-dns", exact: true }) });
  await row.getByRole("button", { name: "Manage component", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Manage external-dns", exact: true });
  await dialog.getByRole("textbox", { name: "Tenant values (YAML object)", exact: true }).fill("# Scale DNS workers\nreplicaCount: 3\n");
  await dialog.getByRole("button", { name: "Validate and review", exact: true }).click();
  await expect(dialog).toContainText("Review update");
  expect(calls.filter(call => call.method === "UpdateComponent" && !call.body.validateOnly)).toHaveLength(0);
  await dialog.getByRole("button", { name: "Apply reviewed update", exact: true }).click();
  await expect.poll(() => calls.filter(call => call.method === "UpdateComponent" && !call.body.validateOnly).length).toBe(1);
  const applied = calls.find(call => call.method === "UpdateComponent" && !call.body.validateOnly)!.body;
  expect(applied.updateMask).toBe("values");
  expect(applied.component).toMatchObject({ id: componentId, etag: "component-etag", values: { replicaCount: 3 } });
});

test("overview shows API inventory and tenant changes clear the previous scope", async ({ page }) => {
  const calls = await mockAPI(page);
  await page.goto("/");
  await expect(page.getByRole("table", { name: "Tenant resource inventory" })).toContainText("Production network");
  await expect(page.getByText("Sample resources only")).toHaveCount(0);
  await page.getByRole("button", { name: /^Tenant acme/ }).click();
  await page.getByRole("option", { name: "other-tenant", exact: true }).click();
  await expect(page.getByRole("button", { name: /^Tenant other-tenant/ })).toBeVisible();
  await expect.poll(() => calls.filter((call) => call.method === "ListNetworks").at(-1)?.body.tenantId).toBe("other-tenant");
  await expect(page.getByRole("table", { name: "Tenant resource inventory" })).toHaveCount(0);
  await expect(page.getByText("No resources were reported for this tenant.", { exact: true })).toBeVisible();
});

test("overview resource names link directly to full detail pages", async ({ page }) => {
  await mockAPI(page);
  await page.goto("/");
  const inventory = page.getByRole("table", { name: "Tenant resource inventory" });
  await expect(inventory.getByRole("link", { name: "Production", exact: true })).toHaveAttribute("href", `/accounts/${accountId}`);
  await expect(inventory.getByRole("link", { name: "Production network", exact: true })).toHaveAttribute("href", `/networks/${networkId}`);
  await expect(inventory.getByRole("link", { name: "payments", exact: true })).toHaveAttribute("href", `/clusters/${clusterId}`);
});

test("overview inventory expands account and network children and reveals matching paths", async ({ page }) => {
  await mockAPI(page);
  await page.goto("/");
  const inventory = page.getByRole("table", { name: "Tenant resource inventory", exact: true });
  await expect(inventory.getByRole("row")).toHaveCount(4);
  await expect(inventory.getByRole("row").filter({ has: page.getByRole("link", { name: "Production", exact: true }) })).toContainText("1 network · 1 cluster");
  await page.getByRole("button", { name: "Collapse Production", exact: true }).click();
  await expect(inventory.getByRole("row")).toHaveCount(2);
  const expand = page.getByRole("button", { name: "Expand Production", exact: true });
  await expect(expand).toHaveAttribute("aria-expanded", "false");
  await expand.focus();
  await expand.press("Enter");
  await expect(inventory.getByRole("row")).toHaveCount(4);
  await page.getByRole("button", { name: "Collapse all", exact: true }).click();
  await page.getByRole("searchbox", { name: "Search resources", exact: true }).fill("payments");
  await expect(inventory.getByRole("row")).toHaveCount(4);
  await expect(page.getByRole("status").filter({ hasText: "1 of 3 resources" })).toBeVisible();
  await expect(inventory.getByText("Context", { exact: true })).toHaveCount(2);
  await page.getByRole("button", { name: "Clear resource filters", exact: true }).click();
  await expect(inventory.getByRole("row")).toHaveCount(2);
  await page.getByRole("button", { name: "Expand all", exact: true }).click();
  await expect(inventory.getByRole("row")).toHaveCount(4);
  await page.screenshot({ path: "/tmp/cv-ui-overview-inventory-tree.png", animations: "disabled" });
});

test("account details expose its managed inventory and related resources", async ({ page }) => {
  await mockAPI(page);
  await page.goto(`/accounts/${accountId}`);
  await expect(page.getByRole("heading", { level: 1 })).toHaveText("Production");
  await page.screenshot({ path: "/tmp/cv-ui-account-overview.png", fullPage: true });
  await page.getByRole("tab", { name: /^Resources\b/ }).click();
  await expect(page.getByRole("table").filter({ hasText: "production-provider" })).toContainText("ProviderConfig");
  await page.getByRole("tab", { name: /^Overview\b/ }).click();
  await expect(page.getByRole("table", { name: "Account related networks" }).getByRole("link", { name: "Production network", exact: true })).toHaveAttribute("href", `/networks/${networkId}`);
  await expect(page.getByRole("link", { name: "payments", exact: true })).toHaveAttribute("href", `/clusters/${clusterId}`);
  await page.reload();
  await page.getByRole("tab", { name: /^Resources\b/ }).click();
  await expect(page.getByRole("table").filter({ hasText: "production-provider" })).toBeVisible();
});

test("network details expose inventory and linked clusters without the create form", async ({ page }) => {
  await mockAPI(page);
  await page.goto(`/networks/${networkId}`);
  await expect(page.getByRole("heading", { level: 1 })).toHaveText("Production network");
  await page.screenshot({ path: "/tmp/cv-ui-network-overview.png", fullPage: true });
  await page.getByRole("tab", { name: /^Resources\b/ }).click();
  await expect(page.getByRole("table", { name: "Network managed resource inventory" })).toContainText("vpc-0123456789abcdef0");
  await page.screenshot({ path: "/tmp/cv-ui-network-resources.png", fullPage: true });
  await page.getByRole("tab", { name: /^Overview\b/ }).click();
  await expect(page.getByRole("link", { name: "payments", exact: true })).toHaveAttribute("href", `/clusters/${clusterId}`);
  await expect(page.getByRole("button", { name: "Create network", exact: true })).toHaveCount(0);
});

test("empty related clusters keeps refresh aligned with the right edge of its heading row", async ({ page }) => {
  await mockAPI(page);
  await page.route("**/api/rpc/cloud.v1.ClusterService/ListClusters", route => route.fulfill({ contentType: "application/json", body: JSON.stringify({ clusters: [] }) }));
  await page.goto(`/networks/${networkId}`);
  await expect(page.getByText("No related clusters reported.", { exact: true })).toBeVisible();
  const heading = page.getByRole("heading", { name: "Related clusters", exact: true });
  const refresh = page.getByRole("button", { name: "Refresh related clusters", exact: true });
  const headingBox = (await heading.boundingBox())!;
  const refreshBox = (await refresh.boundingBox())!;
  const rowBox = (await heading.locator("..").boundingBox())!;
  expect(refreshBox.x).toBeGreaterThan(headingBox.x + headingBox.width);
  expect(Math.abs(refreshBox.x + refreshBox.width - rowBox.x - rowBox.width)).toBeLessThan(2);
  await page.screenshot({ path: "/tmp/cv-network-empty-related-clusters.png", fullPage: true });
});

test("cluster details include all managed resource identifiers and pause state", async ({ page }) => {
  await mockAPI(page);
  await page.goto(`/clusters/${clusterId}`);
  await expect(page.getByRole("heading", { level: 1 })).toHaveText("payments");
  await page.screenshot({ path: "/tmp/cv-ui-cluster-overview.png", fullPage: true });
  await page.getByRole("tab", { name: /^Resources\b/ }).click();
  const resources = page.getByRole("table").filter({ hasText: "payments-workers-external" });
  await expect(resources).toContainText("NodeGroup");
  await expect(resources).toContainText(/paused/i);
  await page.getByRole("tab", { name: /^Overview\b/ }).click();
  await expect(page.getByRole("link", { name: /parent network|Production network|prod-network/i })).toHaveAttribute("href", `/networks/${networkId}`);
});

test("create cluster is a four-step modal that preserves values before submission", async ({ page }) => {
  const calls = await mockAPI(page);
  await page.goto("/clusters");
  await page.getByRole("button", { name: "Create cluster", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Create cluster", exact: true });
  await dialog.getByRole("textbox", { name: /^Cluster name\b/ }).fill("orders");
  await dialog.getByRole("button", { name: /^Existing network\b/ }).click();
  await page.getByRole("option", { name: /Production network/ }).click();
  await dialog.getByRole("button", { name: "Next", exact: true }).click();
  await expect(dialog.getByRole("heading", { name: "Compute", exact: true })).toBeFocused();
  await dialog.getByRole("button", { name: "Back", exact: true }).click();
  await expect(dialog.getByRole("textbox", { name: /^Cluster name\b/ })).toHaveValue("orders");
  await dialog.getByRole("button", { name: "Next", exact: true }).click();
  await dialog.getByRole("button", { name: "Next", exact: true }).click();
  await dialog.getByRole("button", { name: "Next", exact: true }).click();
  await expect(dialog.getByRole("alert")).toContainText("Public API access requires at least one allowed CIDR");
  expect(calls.filter((call) => call.method === "CreateCluster")).toHaveLength(0);
  await dialog.getByRole("textbox", { name: /^Allowed IPv4 CIDRs\b/ }).fill("203.0.113.0/24");
  await dialog.getByRole("button", { name: "Next", exact: true }).click();
  await expect(dialog.getByRole("heading", { name: "Review", exact: true })).toBeFocused();
  await expect(dialog).toContainText("orders");
  await expect(dialog).toContainText("Production network");
  expect(calls.filter((call) => call.method === "CreateCluster")).toHaveLength(0);
  await page.screenshot({ path: "/tmp/cv-ui-create-cluster-review.png" });
  await dialog.getByRole("button", { name: "Create cluster", exact: true }).click();
  await expect.poll(() => calls.filter((call) => call.method === "CreateCluster").length).toBe(1);
  const created = calls.find((call) => call.method === "CreateCluster")!.body.cluster as { name: string; spec: { network: { networkId: string }; tier?: unknown } };
  expect(created.name).toBe("orders");
  expect(created.spec.network.networkId).toBe(networkId);
  expect(created.spec.tier).toBeUndefined();
});

test("create dialog fits a mobile viewport and can be dismissed with Escape", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await mockAPI(page);
  await page.goto("/clusters");
  const trigger = page.getByRole("button", { name: "Create cluster", exact: true });
  await trigger.click();
  const dialog = page.getByRole("dialog", { name: "Create cluster", exact: true });
  await expect(dialog).toBeVisible();
  const bounds = await dialog.boundingBox();
  expect(bounds).not.toBeNull();
  expect(bounds!.x).toBeGreaterThanOrEqual(0);
  expect(bounds!.x + bounds!.width).toBeLessThanOrEqual(390);
  expect(bounds!.height).toBeLessThanOrEqual(844);
  await page.screenshot({ path: "/tmp/cv-ui-create-cluster-mobile.png" });
  await page.keyboard.press("Escape");
  await expect(dialog).toHaveCount(0);
  await expect(trigger).toBeFocused();
});

test("account form sends a generated bootstrap request and reports acceptance", async ({ page }) => {
  const calls = await mockAPI(page);
  await page.goto("/accounts");
  await page.getByRole("button", { name: "Connect account", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Connect cloud account", exact: true });
  await dialog.getByRole("button", { name: "Next", exact: true }).click();
  await dialog.getByRole("textbox", { name: /^AWS account ID\b/ }).fill("111122223333");
  await dialog.getByRole("textbox", { name: /^IAM role name\b/ }).fill("cluster-vending-provisioner");
  await page.screenshot({ path: "/tmp/cv-ui-connect-account.png" });
  await dialog.getByRole("button", { name: "Next", exact: true }).click();
  expect(calls.filter((call) => call.method === "BootstrapTenant")).toHaveLength(0);
  await dialog.getByRole("button", { name: "Connect account", exact: true }).click();
  await expect.poll(() => calls.filter((call) => call.method === "BootstrapTenant").length).toBe(1);
  const body = calls.find((call) => call.method === "BootstrapTenant")!.body;
  expect(body.tenantId).toBe("acme");
  expect((body.account as { aws: { accountId: string } }).aws.accountId).toBe("111122223333");
  await expect(page.getByText(/connection accepted/i).first()).toBeVisible();
});

test("tenant viewers get tenant navigation but cannot bootstrap accounts", async ({ page }) => {
  const calls = await mockAPI(page, { viewer: true });
  await page.goto("/accounts");
  await expect(page.getByRole("button", { name: "Connect account", exact: true })).toBeDisabled();
  await expect(page.getByRole("link", { name: "Freeze controls", exact: true })).toBeVisible();
  await expect(page.getByRole("link", { name: "Releases", exact: true })).toBeVisible();
  expect(calls.filter((call) => call.method === "BootstrapTenant")).toHaveLength(0);
});

test("freeze requires an explicit target review and confirms the returned revision", async ({ page }) => {
  const calls = await mockAPI(page);
  await page.goto("/freeze");
  await page.getByRole("button", { name: "Inspect selected scope", exact: true }).click();
  await expect(page.getByText("Unfrozen", { exact: true })).toBeVisible();
  await page.getByRole("textbox", { name: /^Reason\b/ }).fill("Planned maintenance");
  await page.getByRole("button", { name: "Review freeze", exact: true }).click();
  expect(calls.filter((call) => call.method === "Freeze")).toHaveLength(0);
  await page.getByRole("textbox", { name: "Type exactly: tenant acme", exact: true }).fill("tenant acme");
  await page.getByRole("button", { name: "Confirm freeze for tenant acme", exact: true }).click();
  await expect(page.getByText("Convergence confirmed", { exact: true })).toBeVisible();
  const body = calls.find((call) => call.method === "Freeze")!.body;
  expect(body.target).toEqual({ tenantId: "acme" });
  expect(body.reason).toBe("Planned maintenance");
  expect(calls.filter((call) => call.method === "Freeze")).toHaveLength(1);
});

test("tenant administrators can unfreeze only tenant, network and cluster scopes", async ({ page }) => {
  const calls = await mockAPI(page, { frozen: true, tenantAdmin: true });
  for (const [scope, id, target, phrase] of [
    ["cluster", clusterId, { clusterId }, `cluster ${clusterId}`],
    ["network", networkId, { networkId }, `network ${networkId}`],
    ["tenant", "acme", { tenantId: "acme" }, "tenant acme"],
  ] as const) {
    await page.goto(`/freeze?scope=${scope}${id ? `&id=${id}` : ""}`);
    const before = calls.filter(call => call.method === "Unfreeze").length;
    const action = page.getByRole("button", { name: "Unfreeze scope", exact: true });
    await expect(action).toBeEnabled();
    await action.click();
    const dialog = page.getByRole("dialog", { name: "Confirm unfreeze", exact: true });
    await expect(dialog).toBeVisible();
    expect(calls.filter(call => call.method === "Unfreeze")).toHaveLength(before);
    if (scope === "cluster") await expect(dialog).toContainText("Its shared network keeps its own freeze");
    await dialog.getByRole("textbox", { name: `Type exactly: ${phrase}`, exact: true }).fill(phrase);
    await dialog.getByRole("button", { name: `Confirm unfreeze for ${phrase}`, exact: true }).click();
    await expect(dialog).toHaveCount(0);
    await expect(page.getByText("Convergence confirmed", { exact: true })).toBeVisible();
    expect(calls.filter(call => call.method === "Unfreeze").at(-1)!.body).toEqual({ tenantId: "acme", target });
  }
  await page.screenshot({ path: "/tmp/cv-ui-unfreeze-tenant.png", fullPage: true, animations: "disabled" });
});

test("Unfreeze can reverse a freeze that has not converged", async ({ page }) => {
  const calls = await mockAPI(page, { freezeConverges: false });
  await page.goto("/freeze");
  await page.getByRole("button", { name: "Inspect selected scope", exact: true }).click();
  await expect(page.getByText("Unfrozen", { exact: true })).toBeVisible();
  await page.getByRole("textbox", { name: /^Reason\b/ }).fill("Maintenance cancelled");
  await page.getByRole("button", { name: "Review freeze", exact: true }).click();
  await page.getByRole("textbox", { name: "Type exactly: tenant acme", exact: true }).fill("tenant acme");
  await page.getByRole("button", { name: "Confirm freeze for tenant acme", exact: true }).click();
  await expect(page.getByText("Request accepted; awaiting convergence", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "Unfreeze scope", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Confirm unfreeze", exact: true });
  await expect(dialog.getByRole("textbox")).toBeEnabled();
  await dialog.getByRole("textbox", { name: "Type exactly: tenant acme", exact: true }).fill("tenant acme");
  await dialog.getByRole("button", { name: "Confirm unfreeze for tenant acme", exact: true }).click();
  await expect(page.getByText("Convergence confirmed", { exact: true })).toBeVisible();
  expect(calls.filter(call => call.method === "Unfreeze")).toHaveLength(1);
  expect(calls.find(call => call.method === "Unfreeze")!.body).toEqual({ tenantId: "acme", target: { tenantId: "acme" } });
});

test("resource details link to their own freeze scope without changing policy", async ({ page }) => {
  const calls = await mockAPI(page);
  for (const [url, scope, id] of [[`/clusters/${clusterId}`, "cluster", clusterId], [`/networks/${networkId}`, "network", networkId]]) {
    await page.goto(url);
    await page.getByRole("link", { name: `Freeze ${scope}`, exact: true }).click();
    await expect(page).toHaveURL(new RegExp(`/freeze\\?scope=${scope}&id=${id}`));
    await expect(page.getByRole("button", { name: "Unfreeze scope", exact: true })).toBeEnabled();
    expect(calls.filter(call => call.method === "Freeze" || call.method === "Unfreeze")).toHaveLength(0);
  }
});

test("freeze links reject invalid scopes and stay read-only for viewers", async ({ page }) => {
  const calls = await mockAPI(page, { viewer: true });
  await page.goto(`/clusters/${clusterId}`);
  await expect(page.getByRole("link", { name: /^(Freeze|Unfreeze) cluster$/ })).toHaveCount(0);
  await page.goto(`/freeze?scope=cluster&id=${clusterId}`);
  await expect(page.getByText(/Tenant administrator permission is required/)).toBeVisible();
  await expect.poll(() => calls.filter(call => call.method === "GetFreeze").length).toBeGreaterThan(0);
  await expect(page.getByRole("button", { name: "Unfreeze scope", exact: true })).toHaveCount(0);
  await page.goto("/freeze?scope=cluster&id=invalid");
  await expect(page.getByRole("heading", { name: "404", exact: true })).toBeVisible();
  expect(calls.filter(call => ["Freeze", "Unfreeze"].includes(call.method))).toHaveLength(0);
  expect(calls.filter(call => call.method === "GetFreeze").every(call => call.service === "cloud.v1.TenantService" && call.body.tenantId === "acme")).toBe(true);
});

test("tenant dashboard binds freeze to its tenant and rejects platform scope", async ({ page }) => {
  const calls = await mockAPI(page, { tenantAdmin: true });
  await page.goto("/freeze?scope=tenant&id=another-tenant");
  await expect(page.getByRole("textbox", { name: "Tenant ID", exact: true })).toHaveValue("acme");
  await expect(page.getByRole("textbox", { name: "Tenant ID", exact: true })).toHaveJSProperty("readOnly", true);
  await expect(page.getByText(/This link names another tenant/)).toBeVisible();
  expect(calls.filter(call => call.method === "GetFreeze")).toHaveLength(0);
  await page.getByRole("button", { name: "Inspect selected scope", exact: true }).click();
  await expect(page.getByRole("button", { name: "Unfreeze scope", exact: true })).toBeEnabled();
  expect(calls.filter(call => call.method === "GetFreeze").at(-1)!.body).toEqual({ tenantId: "acme", target: { tenantId: "acme" } });
  await expect(page.getByRole("navigation", { name: "Primary", exact: true }).getByText("Platform", { exact: true })).toHaveCount(0);
  await page.goto("/freeze?scope=platform");
  await expect(page.getByRole("heading", { name: "404", exact: true })).toBeVisible();
  expect(calls.some(call => call.service === "cloud.v1.AdminService")).toBe(false);
  expect(calls.some(call => call.method === "Freeze" || call.method === "Unfreeze")).toBe(false);
});

test("tenant unfreeze does not offer to override an additional platform pause", async ({ page }) => {
  const calls = await mockAPI(page, { tenantAdmin: true, frozen: true, platformPaused: true });
  await page.goto(`/freeze?scope=cluster&id=${clusterId}`);
  await expect(page.getByText("An additional platform pause applies", { exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "Unfreeze scope", exact: true })).toBeDisabled();
  expect(calls.filter(call => call.method === "Unfreeze")).toHaveLength(0);
});

test("network updates review deletions before applying the exact plan ID", async ({ page }) => {
  const calls = await mockAPI(page);
  await page.goto("/networks");
  await page.getByRole("link", { name: "Production network", exact: true }).click();
  await page.getByRole("button", { name: "Edit network", exact: true }).click();
  await page.getByRole("textbox", { name: /^Display name\b/ }).fill("Renamed network");
  const dialog = page.getByRole("dialog", { name: "Edit network", exact: true });
  await dialog.getByRole("button", { name: "Next", exact: true }).click();
  await dialog.getByRole("button", { name: "Back", exact: true }).click();
  await expect(dialog.getByRole("textbox", { name: /^Display name\b/ })).toHaveValue("Renamed network");
  await advanceToReview(dialog);
  expect(calls.filter((call) => call.method === "UpdateNetwork")).toHaveLength(0);
  await dialog.getByRole("button", { name: "Preview changes", exact: true }).click();
  await expect(page.getByRole("table", { name: "Planned resource changes" })).toContainText("old-flow-log");
  expect(calls.filter((call) => call.method === "UpdateNetwork" && !call.body.validateOnly)).toHaveLength(0);
  await page.getByRole("button", { name: "Apply reviewed plan", exact: true }).click();
  await expect.poll(() => calls.filter((call) => call.method === "UpdateNetwork" && !call.body.validateOnly).length).toBe(1);
  const updates = calls.filter((call) => call.method === "UpdateNetwork");
  expect(updates[0].body.validateOnly).toBe(true);
  expect(updates[1].body.planId).toBe("reviewed-network-plan");
  expect((updates[1].body.network as { etag: string }).etag).toBe("network-etag");
  expect(updates[1].body.updateMask).toBe("displayName");
});

test("a stale plan is surfaced without silently replanning or retrying", async ({ page }) => {
  const calls = await mockAPI(page, { failApply: true });
  await page.goto("/networks");
  await page.getByRole("link", { name: "Production network", exact: true }).click();
  await page.getByRole("button", { name: "Edit network", exact: true }).click();
  await page.getByRole("textbox", { name: /^Display name\b/ }).fill("Another network name");
  const dialog = page.getByRole("dialog", { name: "Edit network", exact: true });
  await advanceToReview(dialog);
  await dialog.getByRole("button", { name: "Preview changes", exact: true }).click();
  await page.getByRole("button", { name: "Apply reviewed plan", exact: true }).click();
  await expect(page.getByText(/reviewed plan is stale; plan again/i)).toBeVisible();
  expect(calls.filter((call) => call.method === "UpdateNetwork")).toHaveLength(2);
});


test("disabled add-ons and catalog components never appear Ready", async ({ page }) => {
  await mockAPI(page, { componentCatalog: true });
  await page.route("**/api/rpc/**", async route => {
    const method = new URL(route.request().url()).pathname.split("/").at(-1);
    if (method === "GetCluster") {
      await route.fulfill({ contentType: "application/json", body: JSON.stringify({ cluster: { ...cluster,
        spec: { ...cluster.spec, naas: false, aws: { ...cluster.spec.aws, addons: [{ name: "metrics-server", version: "v1" }] } },
        status: { ...cluster.status, aws: { addons: [
          { name: "metrics-server", version: "v1", state: "MANAGED_RESOURCE_STATE_READY" },
          { name: "aws-efs-csi-driver", version: "v1", state: "MANAGED_RESOURCE_STATE_DELETING" },
          { name: "cert-manager", version: "v1", state: "MANAGED_RESOURCE_STATE_READY" },
        ] }, platformComponents: [{ name: "capsule", phase: "COMPONENT_PHASE_READY", installedVersion: "0.15.2" }] },
      } }) }); return;
    }
    if (method === "ListComponents") {
      await route.fulfill({ contentType: "application/json", body: JSON.stringify({ components: [component,
        { tenantId: "acme", clusterId, component: "policies", management: "COMPONENT_MANAGEMENT_TENANT", status: { phase: "COMPONENT_PHASE_READY" } },
      ] }) }); return;
    }
    await route.fallback();
  });
  await page.goto(`/clusters/${clusterId}`);
  await page.getByRole("tab", { name: "Components", exact: true }).click();
  const addons = page.getByRole("table", { name: "Cluster EKS add-on target and running versions", exact: true });
  const addon = (name: string) => addons.getByRole("row").filter({ has: page.getByText(name, { exact: true }) });
  await expect(addon("snapshot-controller")).toContainText("Disabled");
  await expect(addon("snapshot-controller")).not.toContainText("Ready");
  await expect(addon("metrics-server")).toContainText("Ready");
  await expect(addon("aws-efs-csi-driver")).toContainText("Deleting");
  await expect(addon("cert-manager")).toContainText("Disabling");
  await expect(page.getByRole("table", { name: "Cluster platform component pinned and installed versions", exact: true })).toContainText("Disabling");
  const policies = page.getByRole("table", { name: "Cluster tenant component catalog", exact: true }).getByRole("row").filter({ has: page.getByRole("link", { name: "policies", exact: true }) });
  await expect(policies).toContainText("Disabled");
  await expect(policies.getByRole("button", { name: "Enable component", exact: true })).toBeEnabled();
  await page.goto(`/components/policies?cluster=${clusterId}`);
  await expect(page.getByRole("heading", { level: 1 })).toHaveText("policies");
  await expect(page.getByRole("button", { name: "Enable component", exact: true })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Manage component", exact: true })).toHaveCount(0);
  await page.goto(`/components?cluster=${clusterId}`);
  await expect(page.getByRole("table", { name: "Component catalog", exact: true })).toContainText("policies");
  await expect(page.getByRole("table", { name: "Installed components", exact: true })).toHaveCount(0);
});
