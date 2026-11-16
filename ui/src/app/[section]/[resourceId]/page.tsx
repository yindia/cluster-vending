import type { Metadata } from "next";
import { notFound } from "next/navigation";
import ConsoleApp from "@/components/console/console-app";

type ResourceSection = "accounts" | "networks" | "clusters" | "releases" | "channels" | "components";
type Params = Promise<{ section: string; resourceId: string }>;
type Search = Promise<Record<string, string | string[] | undefined>>;

function isResourceSection(section: string): section is ResourceSection {
  return ["accounts", "networks", "clusters", "releases", "channels", "components"].includes(section);
}

export async function generateMetadata({ params }: { params: Params }): Promise<Metadata> {
  const { section } = await params;
  return { title: isResourceSection(section) ? `${section.slice(0, -1)} details` : "Resource not found" };
}

export default async function ResourceDetailsPage({ params, searchParams }: { params: Params; searchParams: Search }) {
  const { section, resourceId } = await params;
  if (!isResourceSection(section) || !/^[a-zA-Z0-9][a-zA-Z0-9._-]{0,252}$/.test(resourceId)) notFound();
  if (section === "channels") {
    if (resourceId !== "alpha" && resourceId !== "beta" && resourceId !== "stable") notFound();
    return <ConsoleApp section="releases" channelId={resourceId} />;
  }
  const query = await searchParams;
  const clusterId = typeof query.cluster === "string" ? query.cluster : undefined;
  if (clusterId && !/^[a-zA-Z0-9][a-zA-Z0-9._-]{0,252}$/.test(clusterId)) notFound();
  return <ConsoleApp section={section} resourceId={resourceId} componentClusterId={section === "components" ? clusterId : undefined} />;
}
