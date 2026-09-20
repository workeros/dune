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
  await expect(page.locator(".xterm-helper-textarea")).toHaveCount(0);
  state.discoveryIssues.two = "ACCESS_DENIED";
  await page.getByRole("button", { name: "刷新", exact: true }).click();
  await expect(page.locator(".xterm-helper-textarea")).toHaveCount(0);
  await expect(page.getByRole("button", { name: "打开 A-ACP · Runner one", exact: true })).toBeVisible();
  expect(state.connections).toHaveLength(2);
});

test("partial discovery retains healthy panes and marks only missing cached targets unavailable", async ({ page }) => {
  const state = workbenchState();
  await mockWorkbench(page, state); await page.goto("/");
  await page.getByRole("button", { name: "打开 A-ACP · Runner one", exact: true }).click();
  await page.getByRole("button", { name: "打开 A-PTY · Runner one", exact: true }).click();
  await expect.poll(() => state.connections.length).toBe(2);
  const missing = state.runtimes.one.find((runtime) => runtime.adapter === "pty")!;
  state.runtimes.one = state.runtimes.one.filter((runtime) => runtime !== missing);
  state.runtimeIssues = [{ runner_id: "one", runtime: missing, code: "REGISTRATION_INVALID" }];
  for (let i = 0; i < 3; i++) {
    await page.getByRole("button", { name: "刷新", exact: true }).click();
    await expect(page.getByText("Runner one：部分会话的注册信息无法核实", { exact: true })).toBeVisible();
    await expect(page.getByRole("button", { name: "打开 A-ACP · Runner one", exact: true })).toHaveCount(1);
    await expect(page.getByRole("button", { name: "打开 A-PTY · Runner one", exact: true })).toHaveCount(1);
  }
  await expect(page.locator(".xterm-helper-textarea")).toHaveCount(0);
  await expect(page.getByText("原会话已结束，可以新建 Agent。", { exact: true })).toHaveCount(0);
  expect(state.connections).toHaveLength(2);
  state.runtimes.one.push(missing); state.runtimeIssues = [];
  await page.getByRole("button", { name: "刷新", exact: true }).click();
  await expect(page.locator(".xterm-helper-textarea")).toHaveCount(1);
  await expect.poll(() => state.connections.length).toBe(3);
  expect(state.connections[2]).toBe(state.connections[1]);
  expect(state.starts).toBe(0);
  expect(state.inputs.filter((input) => input.message.type !== "resize")).toHaveLength(0);
});
