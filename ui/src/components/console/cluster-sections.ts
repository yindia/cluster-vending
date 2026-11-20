const groups = [
  { label: "Overview", pages: [{ value: "overview", label: "Overview" }] },
  { label: "Infrastructure", pages: [
    { value: "node-groups", label: "Node groups" },
    { value: "topology", label: "Topology" },
    { value: "resources", label: "Infrastructure inventory" },
  ] },
  { label: "Services & tenancy", pages: [
    { value: "components", label: "Components & add-ons" },
    { value: "cloud-resources", label: "Platform cloud resources" },
    { value: "subtenants", label: "Sub Tenants" },
  ] },
  { label: "Operations", pages: [
    { value: "activity", label: "Activity" },
    { value: "violations", label: "Policy violations" },
    { value: "deprecations", label: "Deprecated APIs" },
    { value: "recommendations", label: "Recommendations" },
  ] },
  { label: "Configuration", pages: [{ value: "configuration", label: "Configuration" }] },
];

export function clusterSections(naas: boolean) {
  return groups.map(group => ({ ...group, pages: group.pages.filter(page => page.value !== "subtenants" || naas) }));
}
