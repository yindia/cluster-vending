import { notFound } from "next/navigation";
import ConsoleApp from "@/components/console/console-app";

export const metadata = { title: "Node group details" };

export default async function NodeGroupPage({ params }: PageProps<"/[section]/[resourceId]/node-groups/[group]">) {
  const { section, resourceId, group } = await params;
  const validId = /^[a-zA-Z0-9][a-zA-Z0-9._-]{0,252}$/;
  // A node group is a lower-case DNS-style name (the API's pattern), at most 64 characters.
  if (section !== "clusters" || !validId.test(resourceId) || !/^[a-z0-9]([a-z0-9._-]{0,62}[a-z0-9])?$/.test(group)) notFound();
  return <ConsoleApp section="clusters" resourceId={resourceId} nodeGroup={group} />;
}
