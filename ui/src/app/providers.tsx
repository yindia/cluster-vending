"use client";

import { createContext, useContext, useState, type ReactNode } from "react";
import NextLink from "next/link";
import { Theme } from "@astryxdesign/core/theme";
import { LinkProvider } from "@astryxdesign/core/Link";
import { themes, defaultThemeSlug } from "../astryx-themes";
import { PlatformProvider } from "@/components/platform-provider";
import { AuthBoundary } from "@/components/auth-boundary";

type Mode = "light" | "dark" | "system";
const Appearance = createContext<{ mode: Mode; setMode: (value: string) => void } | null>(null);

export function Providers({ children, authEnabled = false }: { children: ReactNode; authEnabled?: boolean }) {
  const [mode, setMode] = useState<Mode>("light");
  const changeMode = (value: string) => {
    if (value === "light" || value === "dark" || value === "system") setMode(value);
  };
  return <Appearance value={{ mode, setMode: changeMode }}>
    <Theme theme={themes[defaultThemeSlug]} mode={mode}>
      <LinkProvider component={NextLink}><AuthBoundary enabled={authEnabled}><PlatformProvider>{children}</PlatformProvider></AuthBoundary></LinkProvider>
    </Theme>
  </Appearance>;
}

export function useAppearance() {
  const context = useContext(Appearance);
  if (!context) throw new Error("Appearance must be used within Providers");
  return context;
}
