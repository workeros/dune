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

test("unopened titles use full directory snapshots and recover by batch discovery", async ({ page }) => {
  const state = workbenchState();
  await mockWorkbench(page, state); await page.goto("/");
  await expect(page.getByRole("button", { name: "打开 A-ACP · Runner one", exact: true })).toBeVisible();
  const runtime = state.runtimes.one.find((item) => item.adapter === "acp")!;
  const runner = state.runners[0];
  const emitTitle = async (revision: string, title: string | null) => {
    await page.evaluate(({ runtime, runner, revision, title }) => {
      const stream = (window as any).directoryStreams.at(-1);
      stream.emit({ kind: "member", agent: { agent_ref: `ref-${runtime.id}`, runner, target: { binding: runner.binding, runtime: { id: runtime.id, incarnation: runtime.incarnation, generation: runtime.generation, adapter: runtime.adapter } }, runtime: { ...runtime, session_metadata: { revision, conversation_id: "current", title } } } });
    }, { runtime, runner, revision, title });
  };
  const initialPages = state.directoryRequests.length;
  await emitTitle("9007199254740994", "修复登录失败");
  await expect(page.getByRole("button", { name: "打开 修复登录失败 · Runner one", exact: true })).toBeVisible();
  await emitTitle("9007199254740993", "迟到旧标题");
  await expect(page.getByRole("button", { name: "打开 修复登录失败 · Runner one", exact: true })).toBeVisible();
  await emitTitle("9007199254740995", null);
  await expect(page.getByRole("button", { name: "打开 A-ACP · Runner one", exact: true })).toBeVisible();
  expect(state.directoryRequests).toHaveLength(initialPages);
  expect(state.connections).toHaveLength(0);
  expect(state.calls.filter((call) => call.operation !== "machine.info")).toHaveLength(0);
  runtime.session_metadata = { revision: "9007199254740999", conversation_id: "current", title: "断线后的最终标题" };
  await page.evaluate(() => (window as any).directoryStreams.at(-1).onerror());
  await expect(page.getByRole("button", { name: "打开 断线后的最终标题 · Runner one", exact: true })).toBeVisible();
  expect(state.directoryRequests.length).toBeGreaterThan(initialPages);
  expect(state.connections).toHaveLength(0);
  expect(state.calls.filter((call) => call.operation !== "machine.info")).toHaveLength(0);
  expect(state.starts).toBe(0);
});
