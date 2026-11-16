export const sections = ["accounts", "networks", "clusters", "components", "releases", "freeze"] as const;
export type Section = "overview" | (typeof sections)[number];
