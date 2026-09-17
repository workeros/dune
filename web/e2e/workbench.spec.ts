import { test, expect } from "@playwright/test";
import { mockWorkbench, workbenchState } from "./workbench-fixture";
import { addPane, leaves } from "../src/workbench/model";

test("four mixed Agents split across projects and Runners, retain connections and restore", async ({ page, browser }) => {
  const state = workbenchState();
  await mockWorkbench(page, state); await page.goto("/");
  await expect(page.getByText("布局已保存", { exact: true })).toBeVisible();
  for (const label of ["A-PTY · Runner one", "A-ACP · Runner one", "B-PTY · Runner two", "B-ACP · Runner two"]) await page.getByRole("button", { name: `打开 ${label}`, exact: true }).click();
  await expect(page.locator(".agent-pane")).toHaveCount(4);
  await expect.poll(() => state.connections.length).toBe(4);
  await page.getByRole("button", { name: "打开 A-PTY · Runner one", exact: true }).click();
  await expect(page.locator(".agent-pane")).toHaveCount(4);
  const pane = page.locator(".agent-pane").filter({ has: page.locator(".pane-title strong", { hasText: "A-PTY" }) });
  const terminal = pane.locator(".xterm-helper-textarea");
  await terminal.focus(); await page.keyboard.type("echo one");
  await expect.poll(() => state.inputs.filter((input) => input.message.type === "input").length).toBeGreaterThan(0);
  expect(state.inputs.filter((input) => input.message.type === "input").every((input) => input.path.includes("one-pty"))).toBeTruthy();
  await page.getByRole("separator").first().focus(); await page.keyboard.press("ArrowRight");
  await expect(page.getByRole("separator").first()).toHaveAttribute("aria-valuenow", "55");
  expect(state.connections).toHaveLength(4);
  await page.getByRole("button", { name: "Project A", exact: true }).click();
  await expect(page.getByRole("navigation", { name: "Agent 列表" }).getByRole("button")).toHaveCount(2);
  await expect(page.locator(".agent-pane")).toHaveCount(4);
  await expect(page.getByText("布局已保存", { exact: true })).toBeVisible();
  const otherBrowser = await browser.newContext();
  const second = await otherBrowser.newPage(); await mockWorkbench(second, state); await second.goto("/");
  await expect(second.locator(".agent-pane")).toHaveCount(4);
  await expect(second.getByRole("separator").first()).toHaveAttribute("aria-valuenow", "55");
  expect(state.starts).toBe(0);
  await otherBrowser.close();
  await page.getByRole("button", { name: "移出 A-PTY", exact: true }).click();
  await expect(page.locator(".agent-pane")).toHaveCount(3);
  expect(state.calls.filter((call) => call.operation === "runtime.stop")).toHaveLength(0);
});

test("review follows focus, can pin another Runner, and stale bindings never attach", async ({ page }) => {
  const state = workbenchState(); await mockWorkbench(page, state); await page.goto("/");
  await page.getByRole("button", { name: "打开 A-PTY · Runner one", exact: true }).click();
  await page.getByRole("button", { name: "打开 B-PTY · Runner two", exact: true }).click();
  await page.getByRole("button", { name: "Git diff", exact: true }).click();
  await expect(page.getByText("diff from two /repo-b", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "固定审阅 A-PTY", exact: true }).click();
  await page.getByRole("button", { name: "打开 B-PTY · Runner two", exact: true }).click();
  await expect(page.getByText("diff from one /repo-a", { exact: true })).toBeVisible();
  await expect(page.getByText("已固定审阅", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "文件", exact: true }).click();
  await expect(page.getByRole("navigation", { name: "审阅文件" })).toBeVisible();
  expect(state.calls.filter((call) => call.operation === "files").at(-1)?.payload.path).toBe("/repo-a");
  await expect(page.getByText("布局已保存", { exact: true })).toBeVisible();
  state.runners[0].binding!.revision++;
  const before = state.connections.filter((path) => path.includes("one-pty")).length;
  await page.reload();
  await expect(page.getByText("原环境绑定已失效，请从列表选择当前会话。", { exact: true })).toBeVisible();
  expect(state.connections.filter((path) => path.includes("one-pty"))).toHaveLength(before);
  expect(state.starts).toBe(0);
});

test("layout conflict preserves local panes without repeatedly overwriting remote state", async ({ page }) => {
  const state = workbenchState(); state.conflict = true;
  await mockWorkbench(page, state); await page.goto("/");
  await page.getByRole("button", { name: "打开 A-ACP · Runner one", exact: true }).click();
  await expect(page.getByRole("alert").filter({ hasText: "另一个窗口已更新布局" })).toBeVisible();
  await expect(page.locator(".agent-pane")).toHaveCount(1);
  await page.getByRole("button", { name: "打开 B-ACP · Runner two", exact: true }).click();
  await expect(page.locator(".agent-pane")).toHaveCount(2);
  await page.waitForTimeout(600);
  expect(state.viewWrites).toHaveLength(1); expect(state.view.root).toBeNull();
  state.conflict = false;
  await page.getByRole("button", { name: "加载已保存布局", exact: true }).click();
  await expect(page.locator(".agent-pane")).toHaveCount(0);
});

test("project editing supports multiple Runner directories; narrow view keeps one visible input", async ({ page }) => {
  const state = workbenchState(); await mockWorkbench(page, state); await page.goto("/");
  await page.getByRole("button", { name: "新建项目", exact: true }).click();
  await page.getByLabel("项目名称", { exact: true }).fill("Shared work");
  await page.getByRole("button", { name: "添加目录", exact: true }).click();
  await page.getByLabel("目录 1 的路径").fill("/repo-a");
  await page.getByRole("button", { name: "添加目录", exact: true }).click();
  await page.getByLabel("目录 2 的开发环境").selectOption({ label: "Runner two" });
  await page.getByLabel("目录 2 的路径").fill("/repo-b");
  await page.getByRole("button", { name: "保存项目", exact: true }).click();
  await expect(page.getByRole("button", { name: "Shared work", exact: true })).toBeVisible();
  expect(state.projects.at(-1)?.directories.map((directory) => directory.binding.runner_id)).toEqual(["one", "two"]);
  await page.getByRole("button", { name: "全部项目", exact: true }).click();
  await page.getByRole("button", { name: "打开 A-PTY · Runner one", exact: true }).click();
  await page.getByRole("button", { name: "打开 B-ACP · Runner two", exact: true }).click();
  await expect.poll(() => state.connections.length).toBe(2);
  await page.setViewportSize({ width: 600, height: 900 });
  await expect(page.locator(".agent-pane:visible")).toHaveCount(1);
  await expect(page.getByRole("button", { name: "打开 A-PTY · Runner one", exact: true })).toBeVisible();
  await page.getByRole("button", { name: "打开 A-PTY · Runner one", exact: true }).click();
  await expect(page.locator(".agent-pane:visible .pane-title strong")).toHaveText("A-PTY");
  expect(state.inputs.filter((input) => input.message.type === "resize").every((input) => input.message.cols > 0 && input.message.rows > 0)).toBeTruthy();
});

test("a delayed save cannot overwrite changes made during the request", async ({ page }) => {
  const state = workbenchState();
  await mockWorkbench(page, state);
  let release!: () => void;
  const barrier = new Promise<void>((resolve) => { release = resolve; });
  let waiting = false;
  await page.route("**/workbench/views/main", async (route) => {
    if (route.request().method() === "PUT" && !waiting) { waiting = true; await barrier; }
    await route.fallback();
  });
  await page.goto("/");
  await page.getByRole("button", { name: "打开 A-PTY · Runner one", exact: true }).click();
  await expect.poll(() => waiting).toBeTruthy();
  await page.getByRole("button", { name: "打开 B-ACP · Runner two", exact: true }).click();
  release();
  await expect(page.getByText("布局已保存", { exact: true })).toBeVisible();
  expect(leaves(state.view.root)).toHaveLength(2);
  expect(state.viewWrites.map((view) => view.revision)).toEqual([0, 1]);
});

test("summary refresh errors retain open sessions and their connections", async ({ page }) => {
  const state = workbenchState(); await mockWorkbench(page, state); await page.goto("/");
  await page.getByRole("button", { name: "打开 A-PTY · Runner one", exact: true }).click();
  await expect.poll(() => state.connections.length).toBe(1);
  state.discoveryError = true;
  await page.getByRole("button", { name: "刷新", exact: true }).click();
  await expect(page.getByText("Runner one：暂时无法读取 Agent 列表", { exact: true })).toBeVisible();
  await expect(page.locator(".xterm-helper-textarea")).toHaveCount(1);
  state.discoveryError = false;
  await page.getByRole("button", { name: "刷新", exact: true }).click();
  await expect(page.getByText("Runner one：暂时无法读取 Agent 列表", { exact: true })).toHaveCount(0);
  expect(state.connections).toHaveLength(1);
});

test("four panes remain readable with long labels, and a saved view without focus works on mobile", async ({ page }, testInfo) => {
  const state = workbenchState();
  state.projects[0].name = "A project with a deliberately long name for cross Runner collaboration";
  let view = state.view;
  let sequence = 0;
  for (const [index, runtime] of [...state.runtimes.one, ...state.runtimes.two].entries()) {
    if (index === 2) view = { ...view, focus_pane: leaves(view.root)[0].id };
    if (index === 3) view = { ...view, focus_pane: leaves(view.root)[2].id };
    const runner = state.runners[index < 2 ? 0 : 1];
    view = { ...view, ...addPane(view, { target: { binding: runner.binding!, runtime }, project_id: state.projects[index < 2 ? 0 : 1].id }, index > 1 ? "vertical" : "horizontal", () => String(++sequence)) };
  }
  state.view = { ...view, focus_pane: undefined };
  const errors: string[] = []; page.on("pageerror", (error) => errors.push(error.message));
  await mockWorkbench(page, state); await page.goto("/");
  await expect.poll(() => state.connections.length).toBe(4);
  await expect(page.locator(".agent-pane.is-focused")).toHaveCount(1);
  await page.screenshot({ path: testInfo.outputPath("wide.png"), fullPage: true });
  await page.setViewportSize({ width: 600, height: 900 });
  await expect(page.locator(".agent-pane:visible")).toHaveCount(1);
  await page.screenshot({ path: testInfo.outputPath("narrow.png"), fullPage: true });
  expect(errors).toEqual([]);
});
