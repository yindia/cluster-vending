export const clusterSections = [
  { label: "Overview", pages: [{ value: "overview", label: "Overview" }] },
  { label: "Infrastructure", pages: [
    { value: "node-groups", label: "Node groups" },
    { value: "topology", label: "Topology" },
    { value: "resources", label: "Infrastructure inventory" },
  ] },
  { label: "Services", pages: [{ value: "components", label: "Components & add-ons" }] },
  { label: "Operations", pages: [
    { value: "activity", label: "Activity" },
    { value: "violations", label: "Policy violations" },
    { value: "deprecations", label: "Deprecated APIs" },
    { value: "recommendations", label: "Recommendations" },
  ] },
  { label: "Configuration", pages: [{ value: "configuration", label: "Configuration" }] },
];
