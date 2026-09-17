import { defineConfig, devices } from "@playwright/test";

export default defineConfig({
  testDir: "./e2e", timeout: 45_000, expect: { timeout: 8_000 }, fullyParallel: false,
  use: { baseURL: "http://127.0.0.1:5173", trace: "retain-on-failure" },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"], viewport: { width: 1600, height: 1000 } } }],
  webServer: { command: "npx rspack serve --mode production --config rspack.e2e.config.mjs", url: "http://127.0.0.1:5173", reuseExistingServer: false, timeout: 120_000 },
});
