import { notFound } from "next/navigation";
import ConsoleApp from "@/components/console/console-app";

export const metadata = { title: "Sub Tenant details" };

export default async function SubTenantPage({ params }: PageProps<"/[section]/[resourceId]/subtenants/[subtenantId]">) {
  const { section, resourceId, subtenantId } = await params;
  const validId = /^[a-zA-Z0-9][a-zA-Z0-9._-]{0,252}$/;
  if (section !== "clusters" || !validId.test(resourceId) || !validId.test(subtenantId)) notFound();
  return <ConsoleApp section="clusters" resourceId={resourceId} subtenantId={subtenantId} />;
}
