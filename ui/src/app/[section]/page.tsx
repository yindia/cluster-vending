import type { Metadata } from "next";
import { notFound } from "next/navigation";
import ConsoleApp from "@/components/console/console-app";
import type { FreezeSelection } from "@/components/console/freeze-page";
import { sections, type Section } from "@/lib/navigation";

export function generateStaticParams() {
  return sections.map((section) => ({ section }));
}

export async function generateMetadata({ params }: { params: Promise<{ section: string }> }): Promise<Metadata> {
  const { section } = await params;
  return { title: section.charAt(0).toUpperCase() + section.slice(1) };
}

export default async function ResourcePreview({ params, searchParams }: { params: Promise<{ section: string }>; searchParams: Promise<Record<string, string | string[] | undefined>> }) {
  const { section } = await params;
  if (!(sections as readonly string[]).includes(section)) notFound();
  const query = section === "components" || section === "freeze" ? await searchParams : {};
  const clusterId = section === "components" && typeof query.cluster === "string" ? query.cluster : undefined;
  if (clusterId && !/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(clusterId)) notFound();
  let freezeSelection: FreezeSelection | undefined;
  if (section === "freeze" && (query.scope !== undefined || query.id !== undefined)) {
    const scope = query.scope;
    const id = query.id;
    if (typeof scope !== "string" || id !== undefined && typeof id !== "string") notFound();
    switch (scope) {
      case "tenant":
        if (typeof id !== "string" || !/^[a-z0-9]([a-z0-9-]{0,54}[a-z0-9])?$/.test(id)) notFound();
        freezeSelection = { scope, id };
        break;
      case "cluster":
      case "network":
        if (typeof id !== "string" || !/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(id)) notFound();
        freezeSelection = { scope, id };
        break;
      default:
        notFound();
    }
  }
  return <ConsoleApp section={section as Section} componentClusterId={clusterId} freezeSelection={freezeSelection} />;
}
