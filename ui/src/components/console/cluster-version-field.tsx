"use client";

import { useCallback } from "react";
import { Selector } from "@astryxdesign/core/Selector";
import { TextInput } from "@astryxdesign/core/TextInput";
import { usePlatform } from "@/components/platform-provider";
import { useAsyncQuery } from "@/lib/api/use-query";
import {
  findVersionOption, kubernetesVersionOptions, offerChannel, offeredRelease, versionOptionDescription, versionOptionLabel,
  type KubernetesVersionOption,
} from "@/lib/workflows/kubernetes-versions";

export interface KubernetesOffer {
  options: readonly KubernetesVersionOption[];
  release?: string;
  loading: boolean;
  error: string;
}

/** The Kubernetes minors of the release the cluster's channel (else stable) offers, as the server checks them. */
export function useKubernetesOffer(releaseChannel: string): KubernetesOffer {
  const { api, tenantId } = usePlatform();
  const channel = String(offerChannel(releaseChannel));
  const load = useCallback(async (signal: AbortSignal) => {
    const { channels } = await api.tenant.listReleaseChannels({ tenantId }, { signal });
    const name = offeredRelease(channels, channel);
    if (!name) return { release: undefined, options: [] };
    const { release } = await api.tenant.getRelease({ tenantId, name }, { signal });
    return { release: name, options: kubernetesVersionOptions(release?.kubernetes ?? []) };
  }, [api, tenantId, channel]);
  const query = useAsyncQuery(load);
  return { options: query.data?.options ?? [], release: query.data?.release, loading: query.loading, error: query.error };
}

export interface KubernetesVersionFieldProps {
  value: string;
  offer: KubernetesOffer;
  running?: string;
  disabled: boolean;
  onChange: (version: string, option: KubernetesVersionOption | undefined) => void;
}

const plain = { autoComplete: "off", autoCapitalize: "none", hasAutoCorrect: false, hasSpellCheck: false } as const;

/** The Kubernetes minor, chosen from the release's offer. Free text only when the offer cannot be read. */
export function KubernetesVersionField({ value, offer, running, disabled, onChange }: KubernetesVersionFieldProps) {
  const runningNote = running ? ` Running ${running}.` : "";
  if (offer.error || (!offer.loading && !offer.options.length)) {
    const why = offer.error ? "The release's Kubernetes minors could not be loaded" : "The release channel offers no release yet";
    return <TextInput label="Kubernetes minor version" htmlName="kubernetesVersion" value={value} onChange={(next) => onChange(next, undefined)} isDisabled={disabled} {...plain}
      placeholder={running ? `Server default (running ${running})` : "Server default"}
      description={`${why}; type a minor such as 1.34, or leave empty for the server default. A minor on no release channel (extended support) runs with Manual upgrades only.${runningNote}`} />;
  }
  const fallback = offer.options.find((item) => item.isDefault);
  const known = findVersionOption(offer.options, value);
  const options = [
    { value: "", label: fallback ? `Server default (${fallback.minor})` : "Server default", description: "The minor the server picks for a cluster on no channel." },
    ...offer.options.map((item) => ({ value: item.minor, label: versionOptionLabel(item), description: versionOptionDescription(item) })),
    ...(value && !known ? [{ value, label: `${value} · not offered by the release`, description: "The release offers no such minor; the server may refuse it." }] : []),
  ];
  const selected = value && known ? known.minor : value;
  return <Selector label="Kubernetes minor version" htmlName="kubernetesVersion" value={selected} options={options} isDisabled={disabled || offer.loading}
    placeholder={offer.loading ? "Loading the release's minors…" : "Server default"}
    onChange={(next) => onChange(next, findVersionOption(offer.options, next))}
    description={`Minors of release ${offer.release ?? "…"}, with their channel and end of support. A minor on no channel is on extended support.${runningNote}`} />;
}
