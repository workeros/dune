import { expect, test } from "@playwright/test";
import { mockWorkbench, workbenchState } from "./workbench-fixture";

test("shared discovery follows pages without reconnecting panes", async ({ page }) => {
  const state = workbenchState(); state.directoryPageSize = 1;
  await mockWorkbench(page, state); await page.goto("/");
  await page.getByRole("button", { name: "打开 A-ACP · Runner one", exact: true }).click();
  await page.getByRole("button", { name: "打开 B-PTY · Runner two", exact: true }).click();
  await expect.poll(() => state.connections.length).toBe(2);
  expect(state.directoryRequests).toContain("1");
  expect(state.calls.some((call) => call.operation === "runtime.list")).toBe(false);
  await page.getByRole("button", { name: "刷新", exact: true }).click();
  expect(state.connections).toHaveLength(2);
  state.discoveryIssues.two = "RUNNER_UNAVAILABLE";
  await page.getByRole("button", { name: "刷新", exact: true }).click();
  await expect(page.getByText("Runner two：暂时无法读取 Agent 列表", { exact: true })).toBeVisible();
  await expect(page.locator(".xterm-helper-textarea")).toHaveCount(1);
  state.discoveryIssues.two = "ACCESS_DENIED";
  await page.getByRole("button", { name: "刷新", exact: true }).click();
  await expect(page.locator(".xterm-helper-textarea")).toHaveCount(0);
  await expect(page.getByRole("button", { name: "打开 A-ACP · Runner one", exact: true })).toBeVisible();
  expect(state.connections).toHaveLength(2);
});
