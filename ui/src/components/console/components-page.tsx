"use client";

import { SearchFilters } from "./filter-row";

import { useCallback, useEffect, useId, useRef, useState, type FormEvent } from "react";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { Collapsible } from "@astryxdesign/core/Collapsible";
import { Heading } from "@astryxdesign/core/Heading";
import { HStack } from "@astryxdesign/core/HStack";
import { Link } from "@astryxdesign/core/Link";
import { Pagination } from "@astryxdesign/core/Pagination";
import { Selector } from "@astryxdesign/core/Selector";
import { proportional } from "@astryxdesign/core/Table";
import { ConsoleTable as Table } from "./console-table";
import { DetailNavigation, useDetailSection } from "./detail-navigation";
import { Text } from "@astryxdesign/core/Text";
import { TextInput } from "@astryxdesign/core/TextInput";
import { VStack } from "@astryxdesign/core/VStack";
import { stringify } from "yaml";
import { usePlatform } from "@/components/platform-provider";
import { RefreshButton } from "./refresh-button";
import { errorMessage, useAsyncQuery } from "@/lib/api/use-query";
import { CloudProvider, ComponentManagement, ComponentPhase, type Component } from "@/gen/cloud/v1/component_pb";
import {
  applyComponentReview, componentDisablePhrase, componentReady, observeComponent,
  reviewDisableComponent, reviewEnableComponent, reviewUpdateComponent,
  type ComponentAction, type ComponentMutationContext, type ComponentReview,
} from "@/lib/workflows/components";
import { WizardDialog } from "./wizard-dialog";
import { DetailHeader, DetailSummary } from "./detail-layout";
import { CopyButton, CopyValue } from "./copy-value";
import { ValuesEditor, type ValuesEditorHandle } from "./values-editor";

function enumLabel(value: string | undefined, number: number) {
  return value ? value.toLowerCase().replaceAll("_", " ").replace(/^./, letter => letter.toUpperCase()) : `Unknown (${number})`;
}

function componentHref(name: string) {
  return `/components/${encodeURIComponent(name)}`;
}

// Legacy cluster query parameters never scope a catalog route.
export function ComponentsPage({ resourceId }: { resourceId?: string; clusterId?: string }) {
  const { scopeKey } = usePlatform();
  const key = `${scopeKey}:${resourceId ?? "catalog"}`;
  return resourceId ? <ComponentDetail key={key} name={resourceId} /> : <Catalog key={key} />;
}

function Catalog() {
  const { api, tenantId } = usePlatform();
  const [search, setSearch] = useState("");
  const [management, setManagement] = useState("all");
  const [provider, setProvider] = useState("all");
  const [availability, setAvailability] = useState("all");
  const [page, setPage] = useState(1);
  const load = useCallback((signal: AbortSignal) => api.component.listComponentDefinitions({ tenantId }, { signal }), [api, tenantId]);
  const query = useAsyncQuery(load);
  const definitions = query.data?.definitions ?? [];
  const terms = search.trim().toLowerCase().split(/\s+/).filter(Boolean);
  const filtered = definitions.filter((item) => {
    const text = [item.name, item.description, item.disabledReason, item.requires.join(" "), item.conflictsWithAddons.join(" "),
      enumLabel(ComponentManagement[item.management], item.management), enumLabel(CloudProvider[item.provider], item.provider),
      item.disabled ? "Unavailable" : "Available", item.configurable ? "Configurable" : "Not configurable",
      ...Object.entries(item.versions).flat()].join(" ").toLowerCase();
    return terms.every((term) => text.includes(term)) && (management === "all" || management === String(item.management)) &&
      (provider === "all" || provider === String(item.provider)) && (availability === "all" || item.disabled === (availability === "unavailable"));
  });
  const currentPage = Math.min(Math.max(1, page), Math.max(1, Math.ceil(filtered.length / 25)));
  const visible = filtered.slice((currentPage - 1) * 25, currentPage * 25);
  const hasFilters = !!search || management !== "all" || provider !== "all" || availability !== "all";
  return <VStack gap={4}>
    <DetailHeader title="Component catalog" description="Browse release versions, dependencies, and configuration templates."
      actions={<RefreshButton label="Refresh catalog" isLoading={query.refreshing} onClick={query.refresh} />} />
    {query.error && <Banner status="error" title="Catalog could not be loaded" description={query.error} />}
    {query.loading && <Text role="status">Loading catalog…</Text>}
    <SearchFilters label="Component catalog filters" values={[management, provider, availability]}
      search={<TextInput label="Search component catalog" type="search" placeholder="Name, description, dependency or version" value={search} onChange={(value) => { setSearch(value); setPage(1); }} hasClear autoComplete="off" width="min(100%, 24rem)" />}>
      <Selector label="Catalog management" value={management} onChange={(value) => { setManagement(value); setPage(1); }} options={[{ value: "all", label: "All management" }, ...Object.values(ComponentManagement).filter((value) => typeof value === "number").map((value) => ({ value: String(value), label: enumLabel(ComponentManagement[value], value) }))]} />
      <Selector label="Catalog provider" value={provider} onChange={(value) => { setProvider(value); setPage(1); }} options={[{ value: "all", label: "All providers" }, ...Object.values(CloudProvider).filter((value) => typeof value === "number").map((value) => ({ value: String(value), label: enumLabel(CloudProvider[value], value) }))]} />
      <Selector label="Catalog availability" value={availability} onChange={(value) => { setAvailability(value); setPage(1); }} options={[{ value: "all", label: "All availability" }, { value: "available", label: "Available" }, { value: "unavailable", label: "Unavailable" }]} />
      {hasFilters && <Button label="Clear catalog filters" variant="ghost" onClick={() => { setSearch(""); setManagement("all"); setProvider("all"); setAvailability("all"); setPage(1); }} />}
    </SearchFilters>
    {query.data && <Text role="status" color="secondary">{filtered.length} of {definitions.length} catalog components{query.error ? " · Last successful load; refresh failed" : ""}</Text>}
    {query.data && <Table aria-label="Component catalog" idKey="id" rowIndexStart={(currentPage - 1) * 25 + 1} rowCount={filtered.length} data={visible.map(item => ({ id: item.name, item }))} columns={[
      { key: "name", header: "Component", width: proportional(2, { minWidth: 80 }), renderCell: ({ item }) => <HStack gap={2} vAlign="center"><Link href={componentHref(item.name)}>{item.name}</Link><CopyButton value={item.name} label="Component name" /></HStack> },
      { key: "description", header: "Description", width: proportional(3, { minWidth: 80 }), renderCell: ({ item }) => <Text>{item.description || "No description"}</Text> },
      { key: "management", header: "Management / provider", width: proportional(2, { minWidth: 80 }), renderCell: ({ item }) => <Text>{enumLabel(ComponentManagement[item.management], item.management)} / {enumLabel(CloudProvider[item.provider], item.provider)}</Text> },
      { key: "availability", header: "Availability", width: proportional(3, { minWidth: 80 }), renderCell: ({ item }) => <Text>{item.disabled ? `Unavailable: ${item.disabledReason || "No reason reported"}` : "Available"} · {item.configurable ? "Configurable" : "Not configurable"}</Text> },
      { key: "requires", header: "Dependencies / conflicts", width: proportional(2, { minWidth: 80 }), renderCell: ({ item }) => <Text>Requires: {item.requires.join(", ") || "None"}. Conflicts: {item.conflictsWithAddons.join(", ") || "None"}.</Text> },
      { key: "versions", header: "Release versions", width: proportional(2, { minWidth: 80 }), renderCell: ({ item }) => <VStack gap={2}>{Object.entries(item.versions).map(([minor, version]) => <CopyValue key={minor} value={version} display={`${minor} · ${version}`} label={`Version for Kubernetes ${minor}`} />)}{!Object.keys(item.versions).length && <Text>Not reported</Text>}</VStack> },
    ]} />}
    {query.data && !query.error && !filtered.length && <Text>{definitions.length ? "No catalog components match the current filters." : "No component definitions returned."}</Text>}
    {filtered.length > 25 && <Pagination label="Component catalog pagination" page={currentPage} onChange={setPage} totalItems={filtered.length} pageSize={25} variant="count" />}
  </VStack>;
}

function ComponentDetail({ name }: { name: string }) {
  const { api, tenantId } = usePlatform();
  const tabs = [{ value: "overview", label: "Overview" }, { value: "values", label: "Values" }, { value: "schema", label: "Schema" }];
  const navigation = useDetailSection(tabs);
  const tab = navigation.active;
  const loadDefinition = useCallback((signal: AbortSignal) => api.component.listComponentDefinitions({ tenantId }, { signal }), [api, tenantId]);
  const definitionQuery = useAsyncQuery(loadDefinition);
  const definition = definitionQuery.data?.definitions.find(item => item.name === name);

  return <VStack gap={6}>
    <DetailHeader title={name} description={definition?.description || undefined} metadata={<CopyValue value={name} label="Component name" display={`Component: ${name}`} isSecondary />} actions={!definition && definitionQuery.error && <RefreshButton label="Retry component" onClick={definitionQuery.refresh} />} />
    {definitionQuery.loading && <Text role="status">Loading component…</Text>}
    {definitionQuery.error && <Banner status="error" title="Component catalog unavailable" description={definitionQuery.error} />}
    {definitionQuery.data && !definition && <Banner status="warning" title="Component not found" description="This component is not in the current catalog. Return to components or refresh the catalog." />}
    {definition && <>
      {definition.disabled && <Banner status="warning" title="Catalog availability" description={definition.disabledReason || "This component is unavailable."} />}
      <DetailNavigation resource="Component" pages={tabs} selection={navigation} actions={tab !== "values" && <RefreshButton label="Refresh component" isLoading={definitionQuery.refreshing} onClick={definitionQuery.refresh} />} />
      {tabs.map(item => item.value === tab ? <VStack key={item.value} id={navigation.panelId(tab)} role="tabpanel" aria-label={navigation.label} tabIndex={0} gap={6}>
        {tab === "overview" && <>
          <DetailSummary items={[
            { label: "Management", value: enumLabel(ComponentManagement[definition.management], definition.management) },
            { label: "Provider", value: enumLabel(CloudProvider[definition.provider], definition.provider) },
            { label: "Availability", value: definition.disabled ? "Unavailable" : "Available" },
            { label: "Tenant values", value: definition.configurable ? "Configurable" : "Not configurable" },
          ]} />
          <VStack gap={3}>
            <Heading level={2}>Release versions</Heading>
            {Object.keys(definition.versions).length ? <Table aria-label="Component release versions" idKey="minor" data={Object.entries(definition.versions).map(([minor, version]) => ({ minor, version }))} columns={[
              { key: "minor", header: "Kubernetes version" },
              { key: "version", header: "Release version", renderCell: (row) => <CopyValue value={row.version} label={`Version for Kubernetes ${row.minor}`} /> },
            ]} /> : <Text>No release versions reported.</Text>}
          </VStack>
          <VStack gap={3}>
            <Heading level={2}>Dependencies</Heading>
            {definition.requires.length ? <HStack gap={3} wrap="wrap">{definition.requires.map(required => <Link key={required} href={componentHref(required)}>{required}</Link>)}</HStack> : <Text>No dependencies reported.</Text>}
            <Text>Conflicting addons: {definition.conflictsWithAddons.join(", ") || "None reported"}</Text>
          </VStack>
        </>}
        {tab === "values" && <>
          <ValuesEditor label="Platform locked values" language="yaml" value={stringify(definition.platformValues ?? {})} description="Platform managed settings are read-only and override tenant settings." readOnly />
          <ValuesTemplate name={definition.name} onRefreshDefinition={definitionQuery.refresh} />
        </>}
        {tab === "schema" && <ValuesEditor label="Values schema" language="json" value={JSON.stringify(definition.valuesSchema ?? {}, null, 2)} readOnly />}
      </VStack> : null)}
    </>}
  </VStack>;
}

function ValuesTemplate({ name, onRefreshDefinition }: { name: string; onRefreshDefinition: () => void }) {
  const { api } = usePlatform();
  const load = useCallback((signal: AbortSignal) => api.component.getComponentValues({ name }, { signal }), [api, name]);
  const query = useAsyncQuery(load);
  return <VStack gap={4}>
    <HStack hAlign="between" vAlign="center" gap={3} width="100%"><Heading level={3}>Values template</Heading><RefreshButton label="Refresh values template" isLoading={query.refreshing} onClick={() => { onRefreshDefinition(); query.refresh(); }} /></HStack>
    {query.loading && <Text role="status">Loading values template…</Text>}
    {query.error && <Banner status="error" title="Values template unavailable" description={query.error} />}
    {query.data && <ValuesEditor label="Helm values template" language="yaml" value={query.data.userValues} description={query.data.userValues ? "Catalog template, including its original comments." : "No user values template returned."} readOnly />}
  </VStack>;
}

export function ComponentMutationForm({ context, onAccepted }: { context: ComponentMutationContext; onAccepted: (action: ComponentAction, component: Component) => void }) {
  const { api, canWrite } = usePlatform();
  const { definition, current } = context;
  const enabled = !!current?.id;
  const [action, setAction] = useState<ComponentAction>(enabled ? "update" : "enable");
  const [values, setValues] = useState(() => stringify(current?.values ?? {}));
  const [review, setReview] = useState<ComponentReview>();
  const [confirmation, setConfirmation] = useState("");
  const [error, setError] = useState("");
  const [valuesError, setValuesError] = useState("");
  const [confirmationError, setConfirmationError] = useState("");
  const [busy, setBusy] = useState<"validate" | "apply">();
  const [open, setOpen] = useState(false);
  const [step, setStep] = useState(0);
  const formId = useId();
  const requestId = useRef<string | undefined>(undefined);
  const controller = useRef<AbortController | null>(null);
  const valuesRef = useRef<ValuesEditorHandle>(null);
  const confirmationRef = useRef<HTMLInputElement>(null);
  useEffect(() => () => controller.current?.abort(), []);
  const mutable = canWrite && definition.management === ComponentManagement.TENANT && (!current || current.management === ComponentManagement.TENANT);
  if (!mutable) return <Text>This component is read-only.</Text>;
  if (enabled && !current?.etag) return <Text>This component has no etag. Reload current component before editing or disabling.</Text>;
  if (current?.status?.phase === ComponentPhase.DELETING) return <Text>This component is being removed. Refresh inventory to observe deletion.</Text>;
  function invalidate() {
    controller.current?.abort(); controller.current = null;
    setBusy(undefined); setReview(undefined); setConfirmation(""); setError(""); setValuesError(""); setConfirmationError("");
    requestId.current = undefined;
  }
  async function validate(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!canWrite || controller.current) return;
    const request = new AbortController(); controller.current = request;
    setError(""); setValuesError(""); setReview(undefined); setConfirmation(""); setBusy("validate");
    try {
      let result: ComponentReview;
      if (action === "disable") result = reviewDisableComponent({ ...context, canWrite });
      else if (action === "enable") {
        requestId.current ??= crypto.randomUUID();
        result = await reviewEnableComponent(api, { ...context, canWrite }, values, requestId.current, request.signal);
      } else result = await reviewUpdateComponent(api, { ...context, canWrite }, values, request.signal);
      if (!request.signal.aborted) { setReview(result); setStep(1); }
    } catch (failure) {
      if (!request.signal.aborted) {
        const message = errorMessage(failure); setError(message);
        if (action !== "disable") { setValuesError(message); valuesRef.current?.focus(); }
      }
    } finally {
      if (!request.signal.aborted) { controller.current = null; setBusy(undefined); }
    }
  }
  async function apply(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!canWrite || !review || controller.current) return;
    if (review.action === "disable" && confirmation !== componentDisablePhrase(context)) {
      setConfirmationError("Type the exact tenant, cluster, component name and ID shown below."); confirmationRef.current?.focus(); return;
    }
    const request = new AbortController(); controller.current = request;
    setBusy("apply"); setError("");
    try {
      const component = await applyComponentReview(api, { ...context, canWrite }, review, confirmation, request.signal);
      if (!request.signal.aborted) {
        setReview(undefined); setConfirmation(""); setOpen(false); setStep(0);
        onAccepted(review.action, component);
      }
    } catch (failure) {
      if (!request.signal.aborted) setError(`${errorMessage(failure)} Refresh inventory before retrying if the outcome is uncertain. An enable retry uses the same reviewed request ID.`);
    } finally {
      if (!request.signal.aborted) { controller.current = null; setBusy(undefined); }
    }
  }
  const unavailable = action !== "disable" && definition.disabled;
  const noValues = action === "update" && !definition.configurable;
  function changeOpen(next: boolean) {
    if (controller.current) return;
    if (!next) { invalidate(); setStep(0); }
    setOpen(next);
  }
  return <>
    <Button label={enabled ? "Manage component" : "Enable component"} aria-haspopup="dialog" onClick={() => changeOpen(true)} />
    <WizardDialog isOpen={open} onOpenChange={changeOpen} title={enabled ? `Manage ${definition.name}` : `Enable ${definition.name}`} description={`Tenant ${context.tenantId} · Cluster ${context.clusterId}`} steps={["Configure", "Review and confirm"]} step={step} busy={!!busy} footer={<HStack gap={3} wrap="wrap">
      <Button label="Cancel" type="button" isDisabled={!!busy} onClick={() => changeOpen(false)} />
      {step === 1 && <Button label="Back" type="button" isDisabled={!!busy} onClick={() => { setStep(0); setError(""); setConfirmationError(""); }} />}
      {step === 0 ? <Button label={action === "disable" ? "Review removal" : "Validate and review"} variant={action === "disable" ? "destructive" : "primary"} type="submit" form={`${formId}-configure`} isLoading={busy === "validate"} isDisabled={busy === "apply" || unavailable || noValues} />
        : <Button label={review?.action === "disable" ? "Confirm removal" : `Apply reviewed ${review?.action ?? action}`} type="submit" form={`${formId}-apply`} variant={review?.action === "disable" ? "destructive" : "primary"} isLoading={busy === "apply"} isDisabled={busy === "validate" || !review} />}
    </HStack>}>
    {step === 0 && <form id={`${formId}-configure`} method="post" noValidate aria-label="Validate component change" onSubmit={validate}>
      <VStack gap={4}>
        <Selector label="Component action" htmlName="action" value={action} options={enabled ? [{ value: "update", label: "Update values" }, { value: "disable", label: "Disable (remove)" }] : [{ value: "enable", label: "Enable" }]} isDisabled={!!busy} onChange={value => { invalidate(); setAction(value as ComponentAction); }} />
        {action !== "disable" && <ValuesEditor ref={valuesRef} label="Tenant values (YAML object)" language="yaml" description="YAML or JSON object. Maximum 64 KiB of UTF-8 input and serialized values. Values replace the previous tenant object; platform values take precedence. Use secret references." value={values} readOnly={!definition.configurable} disabled={!!busy} error={valuesError || undefined} onChange={value => { invalidate(); setValues(value); }} />}
        {noValues && <Text>This component has no configurable tenant values. It can still be disabled.</Text>}
        {action === "disable" && <Text>Disable removes the component from the cluster. Components that depend on it can prevent removal. Force removal is not requested.</Text>}
      </VStack>
    </form>}
    {step === 1 && review && <form id={`${formId}-apply`} method="post" noValidate aria-label="Apply reviewed component change" onSubmit={apply}>
      <VStack gap={4}>
        <Heading level={3}>Review {review.action}</Heading>
        <HStack gap={4} wrap="wrap"><CopyValue value={context.tenantId} label="Tenant" /><CopyValue value={context.clusterId} label="Cluster ID" /><CopyValue value={definition.name} label="Component name" /></HStack>
        {current?.id && <HStack gap={4} wrap="wrap"><CopyValue value={current.id} label="Component ID" /><CopyValue value={current.etag} label="Reviewed etag" /></HStack>}
        <Text>{review.action === "disable" ? "Remove this component and its managed resources from the cluster. Dependent components may prevent removal." : review.action === "enable" ? "Enable this component with the settings below. Validation passed; installation starts after you apply." : "Replace this component’s tenant settings with the changes below. Validation passed; applying starts the rollout immediately."}</Text>
        {review.action === "disable" ? <TextInput ref={confirmationRef} label={`Type exactly: ${componentDisablePhrase(context)}`} htmlName="confirmation" value={confirmation} autoCapitalize="none" hasAutoCorrect={false} hasSpellCheck={false} isDisabled={!!busy} status={confirmationError ? { type: "error", message: confirmationError } : undefined} onChange={value => { setConfirmation(value); setConfirmationError(""); setError(""); }} /> : <>
          {review.preview.version ? <CopyValue value={review.preview.version} label="Release version" /> : <Text>Release version: selected by the platform</Text>}
          <ComponentSettingsReview before={current?.values} after={review.preview.values} />
          <Text>Settings without a tenant override use catalog defaults. Platform managed settings take precedence. Applying accepts the request; readiness is checked separately.</Text>
          <Collapsible trigger="Effective settings after validation" defaultIsOpen={false}>
            <ComponentSettingsReview after={review.preview.status?.effectiveValues} effective />
          </Collapsible>
          <Collapsible trigger="Technical details · exact validated request" defaultIsOpen={false}>
            <Text>The reviewed request is preserved for apply. Only validation mode changes when it is sent.</Text>
            <ValuesEditor label="Exact validated request" language="json" value={JSON.stringify(JSON.parse(review.requestJson), null, 2)} readOnly />
          </Collapsible>
        </>}
      </VStack>
    </form>}
    {error && <Banner role="alert" status="error" title="Component action failed" description={error} />}
    </WizardDialog>
  </>;
}

function ComponentSettingsReview({ before, after, effective = false }: { before?: Component["values"]; after?: Component["values"]; effective?: boolean }) {
  function settings(values: Component["values"]) {
    const entries = new Map<string, unknown>();
    const pending = Object.entries(values ?? {}).map(([path, value]) => ({ path, value }));
    while (pending.length) {
      const { path, value } = pending.pop()!;
      if (value && typeof value === "object" && Object.keys(value).length) {
        for (const [key, nested] of Object.entries(value)) pending.push({ path: `${path} / ${Array.isArray(value) ? `item ${Number(key) + 1}` : key}`, value: nested });
      } else entries.set(path, value);
    }
    return entries;
  }
  function display(value: unknown, present: boolean) {
    if (!present) return "Use catalog default";
    if (value === null) return "Clear value";
    if (typeof value === "boolean") return value ? "Yes" : "No";
    if (Array.isArray(value)) return "Empty list";
    if (value && typeof value === "object") return "Empty object";
    return value === "" ? "Empty text" : String(value);
  }
  const previous = settings(before), next = settings(after);
  const rows = [...new Set([...previous.keys(), ...next.keys()])].sort().filter(path => effective || previous.has(path) !== next.has(path) || JSON.stringify(previous.get(path)) !== JSON.stringify(next.get(path))).map(path => ({
    id: path, setting: path, previous: display(previous.get(path), previous.has(path)), next: display(next.get(path), next.has(path)),
  }));
  return rows.length ? <Table searchable aria-label={effective ? "Effective component settings" : "Component setting changes"} idKey="id" textOverflow="wrap" data={rows} columns={[
    { key: "setting", header: "Setting", renderCell: row => <Text>{row.setting}</Text> },
    ...(!effective ? [{ key: "previous", header: "Before", renderCell: (row: typeof rows[number]) => <Text>{row.previous}</Text> }] : []),
    { key: "next", header: effective ? "Effective value" : "After applying", renderCell: row => <Text>{row.next}</Text> },
  ]} /> : <Text>{effective ? "No effective settings were reported." : "No tenant overrides change. Catalog defaults and platform settings continue to apply."}</Text>;
}

export function ComponentObservation({ tenantId, action, accepted, onSettled }: { tenantId: string; action: ComponentAction; accepted: Component; onSettled: () => void }) {
  const { api } = usePlatform();
  const [pending, setPending] = useState(true);
  const terminal = useRef<Awaited<ReturnType<typeof observeComponent>> | undefined>(undefined);
  const load = useCallback(async (signal: AbortSignal) => {
    if (terminal.current) return terminal.current;
    const result = await observeComponent(api, tenantId, accepted.id, action, signal);
    const fresh = result.component && result.component.generation >= accepted.generation && result.component.status && result.component.status.observedGeneration >= accepted.generation;
    if (result.removed || fresh && (result.component!.status!.phase === ComponentPhase.FAILED || action !== "disable" && componentReady(result.component!))) {
      terminal.current = result;
      setPending(false);
      onSettled();
    }
    return result;
  }, [api, tenantId, accepted.id, accepted.generation, action, onSettled]);
  const options = { observeOperation: pending, pollMs: pending ? 2000 : undefined };
  const query = useAsyncQuery(load, options);
  const observed = query.data?.component;
  const ready = !query.error && action !== "disable" && !!observed && observed.generation >= accepted.generation && componentReady(observed);
  const removed = !query.error && action === "disable" && query.data?.removed;
  const failed = observed?.status?.phase === ComponentPhase.FAILED;
  return <VStack gap={4}>
    <Banner status={ready || removed ? "success" : failed ? "error" : "info"} title={removed ? "Removal observed" : ready ? "Ready observed" : failed ? "Reconciliation failed" : "Request accepted; convergence pending"} description={removed ? "The component no longer exists in this scope." : ready ? "The observed READY status has caught up with the accepted generation." : failed ? "The controller reported a failure. Review the message and refresh to check again." : "The desired state was accepted. Status is checked every 2 seconds until completion, for up to 15 minutes. Refresh to continue observing."} />
    <CopyValue value={accepted.id} label="Component ID" />
    <Text>Accepted generation: {String(accepted.generation)}{observed?.status ? ` · Observed phase: ${enumLabel(ComponentPhase[observed.status.phase], observed.status.phase)} · Observed generation: ${String(observed.status.observedGeneration)}` : ""}</Text>
    {(observed?.status?.lastError || observed?.status?.message) && <Text>{observed.status.lastError || observed.status.message}</Text>}
    {query.error && <Banner status="error" title="Component observation failed" description={query.error} />}
    <HStack hAlign="end" vAlign="center" width="100%"><RefreshButton label="Refresh accepted component status" isLoading={query.refreshing} onClick={() => { terminal.current = undefined; setPending(true); query.refresh(); }} /></HStack>
  </VStack>;
}
