import { defineConfig } from "@playwright/test";

export default defineConfig({
  testDir: "./tests/browser",
  fullyParallel: false,
  workers: 1,
  timeout: 30_000,
  expect: { timeout: 10_000 },
  use: {
    baseURL: process.env.CV_UI_TEST_BASE_URL ?? "http://localhost:3000",
    channel: process.env.CV_UI_BROWSER_CHANNEL ?? "chrome",
    headless: true,
    viewport: { width: 1440, height: 1000 },
    trace: "retain-on-failure",
  },
  // Deliberately no webServer: use the existing UI listener, never start another.
});
