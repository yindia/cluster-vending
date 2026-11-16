import type { Metadata, Viewport } from "next";
import "@astryxdesign/core/reset.css";
import "@astryxdesign/core/astryx.css";
import "./dialogs.css";
import "./console-controls.css";
import { Providers } from "./providers";

export const metadata: Metadata = {
  title: { default: "Overview | Cluster Vending", template: "%s | Cluster Vending" },
  description: "Manage cloud accounts, networks, clusters and platform rollouts.",
  robots: { index: false, follow: false },
};
export const viewport: Viewport = { width: "device-width", initialScale: 1, viewportFit: "cover" };

export default function RootLayout({ children }: LayoutProps<"/">) {
  return <html lang="en"><body><Providers>{children}</Providers></body></html>;
}
