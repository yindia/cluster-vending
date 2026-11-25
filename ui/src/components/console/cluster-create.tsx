"use client";

import { useId, useRef, useState } from "react";
import { toJsonString } from "@bufbuild/protobuf";
import { Banner } from "@astryxdesign/core/Banner";
import { Button } from "@astryxdesign/core/Button";
import { Collapsible } from "@astryxdesign/core/Collapsible";
import { HStack } from "@astryxdesign/core/HStack";
import { Text } from "@astryxdesign/core/Text";
import { VStack } from "@astryxdesign/core/VStack";
import { usePlatform } from "@/components/platform-provider";
import { CreateClusterRequestSchema, type CreateClusterRequest } from "@/gen/cloud/v1/cluster_service_pb";
import { errorMessage } from "@/lib/api/use-query";
import { accountDescription } from "@/lib/workflows/cluster-placement";
import {
  buildCreateCluster, clusterProvider, clusterReviewStep, clusterWizardSteps, createRequestIdentity, emptyClusterForm,
  validateClusterForm, validateClusterFormStep, type ClusterForm,
} from "@/lib/workflows/clusters";
import { clusterFormYaml, parseClusterFormYaml } from "@/lib/workflows/cluster-yaml";
import { ClusterFields, useClusterFieldRows } from "./cluster-form-fields";
import { ClusterReview } from "./cluster-form-review";
import { ErrorNotice, NetworkPicker, NetworkPlacement, useClusterNetworks } from "./cluster-network-picker";
import { ClusterYamlPanel, clusterModeLabels, useClusterYamlMode } from "./cluster-yaml-panel";
import { ResourceWorkflowDialog } from "./manifest-create-dialog";

export function CreateCluster({ onClose, onCreated }: { onClose: () => void; onCreated: (id: string) => void }) {
  const { api, tenantId } = usePlatform();
  const [form, setForm] = useState<ClusterForm>({ ...emptyClusterForm });
  // A YAML edit replaces the form: remount the editors that keep row identities.
  const [revision, setRevision] = useState(0);
  const rows = useClusterFieldRows(form);
  const [step, setStep] = useState(0);
  const [review, setReview] = useState<CreateClusterRequest>();
  const [busy, setBusy] = useState(false);
  const locked = useRef(false);
  const [error, setError] = useState("");
  const errorRef = useRef<HTMLElement>(null);
  const formId = useId();
  const identity = useRef(createRequestIdentity(() => crypto.randomUUID()));
  const choices = useClusterNetworks();
  const networkOf = (id: string) => choices.data?.networks.find((item) => item.id === id);
  const network = networkOf(form.networkId);
  function fail(cause: unknown) { setError(errorMessage(cause)); requestAnimationFrame(() => errorRef.current?.focus()); }
  function invalidate() { identity.current.invalidate(); setReview(undefined); setError(""); }
  const yaml = useClusterYamlMode({
    toYaml: () => clusterFormYaml(form, { region: network?.spec?.region ?? "", networkId: form.networkId }),
    fromYaml: (text) => parseClusterFormYaml(text, { tenantId, regionOf: choices.data ? (id) => networkOf(id)?.spec?.region : undefined }),
    onApplied: (next) => { setForm(next); rows.reset(next); setRevision((value) => value + 1); },
    onEdit: invalidate, onError: fail,
  });
  function change<K extends keyof ClusterForm>(field: K, value: ClusterForm[K]) {
    invalidate();
    setForm((old) => ({ ...old, [field]: value }));
  }
  function reviewRequest(draft: ClusterForm) {
    const selected = networkOf(draft.networkId);
    if (!selected || !choices.data) throw new Error("Choose an existing network.");
    setReview(buildCreateCluster(tenantId, draft, selected, choices.data.accounts, identity.current.get()));
  }
  async function submit() {
    if (locked.current) return;
    try {
      if (step < clusterReviewStep) {
        if (yaml.mode === "yaml") {
          const draft = yaml.apply();
          validateClusterForm(draft);
          reviewRequest(draft);
          setError(""); setStep(clusterReviewStep); return;
        }
        rows.validate(step);
        validateClusterFormStep(form, step);
        if (step === 0) {
          if (!network || !choices.data) throw new Error("Choose an existing network.");
          clusterProvider(network, choices.data.accounts);
        }
        if (step === clusterReviewStep - 1) reviewRequest(form);
        setError(""); setStep(step + 1); return;
      }
      if (!review) throw new Error("Review the create request before submitting.");
      locked.current = true; setBusy(true); setError("");
      const result = await api.cluster.createCluster(review);
      if (!result.cluster?.id) throw new Error("The server returned no cluster ID. Retry unchanged inputs with the same request ID.");
      onCreated(result.cluster.id);
    } catch (cause) { fail(cause); }
    finally { locked.current = false; setBusy(false); }
  }
  const reviewing = step === clusterReviewStep;
  return <ResourceWorkflowDialog isOpen onOpenChange={(open) => { if (!open && !locked.current) onClose(); }} title="Create cluster" steps={clusterWizardSteps} step={step} busy={busy}
    method={{ label: "Edit as", value: yaml.mode, onChange: yaml.switchMode, labels: clusterModeLabels, inline: true }}
    placement={<NetworkPlacement network={network} choices={choices} />}
    description={`Provision a managed cluster in tenant ${tenantId}.`}
    footer={<HStack gap={3} wrap="wrap">
      <Button label="Cancel creation" variant="secondary" onClick={onClose} isDisabled={busy} />
      {step > 0 && (reviewing || yaml.mode === "guided") && <Button label="Back" variant="secondary" onClick={() => { setStep(yaml.mode === "yaml" ? clusterReviewStep - 1 : step - 1); setError(""); }} isDisabled={busy} />}
      <Button label={reviewing ? "Create cluster" : yaml.mode === "yaml" ? "Review" : "Next"} variant="primary" type="submit" form={formId} isLoading={busy} />
    </HStack>}>
    <form id={formId} method="post" aria-label="Cluster creation settings" noValidate onSubmit={(event) => { event.preventDefault(); void submit(); }}>
      <VStack gap={5}>
        {error && <VStack ref={errorRef} tabIndex={-1} role="alert"><ErrorNotice message={error} /></VStack>}
        {!reviewing && yaml.mode === "yaml" && <ClusterYamlPanel value={yaml.text} onChange={yaml.edit} error={yaml.error} disabled={busy} editing={false} />}
        {!reviewing && yaml.mode === "guided" && <ClusterFields key={revision} step={step} value={form} onChange={change} rows={rows} disabled={busy} creating region={network?.spec?.region ?? "Select a network"} tier={network?.spec?.tier}
          networkPicker={<NetworkPicker operation="cluster.create" choices={choices} value={form.networkId} disabled={busy} onChange={(next) => change("networkId", next)} />} />}
        {reviewing && <>
          <ClusterReview form={form} region={network?.spec?.region ?? ""} networkName={network?.displayName || network?.name || form.networkId} accountName={accountDescription(choices.data?.accounts.find((account) => account.id === network?.spec?.bootstrapId))} tier={network?.spec?.tier} />
          <Banner status="info" title="Creates managed cloud resources" description="The controller provisions the control plane and node groups asynchronously. Readiness appears on the detail page; cloud charges may apply." />
          <Collapsible trigger="Advanced · exact API request" defaultIsOpen={false}><VStack as="pre" isScrollable><Text type="code">{review && toJsonString(CreateClusterRequestSchema, review, { prettySpaces: 2 })}</Text></VStack></Collapsible>
        </>}
      </VStack>
    </form>
  </ResourceWorkflowDialog>;
}
