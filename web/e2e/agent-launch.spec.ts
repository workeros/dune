import { test, expect } from "@playwright/test";
import { mockWorkbench, workbenchState } from "./workbench-fixture";
import { leaves } from "../src/workbench/model";

test("starts from a fixed project Profile in a new worktree and restores its project association", async ({ page, browser }, testInfo) => {
  const state = workbenchState();
  state.profiles.push({ id: "chosen", owner_id: "owner", name: "ACP fixture", description: "", revision: 4, created_by: { type: "user", subject: "owner" }, created_at: "", updated_at: "", profile: { version: 1, kind: "agent", adapter: "acp", working_directory: "/profile-default", setup: { steps: [] }, start: { argv: ["fixture", "--acp"] } } });
  state.projects[0].default_profile = { id: "chosen", revision: 2 };
  await mockWorkbench(page, state); await page.goto("/");
  await page.getByRole("button", { name: "Project A", exact: true }).click();
  await page.getByRole("combobox", { name: "工作位置", exact: true }).selectOption("worktree");
  await expect(page.getByRole("button", { name: "启动 Agent", exact: true })).toBeDisabled();
  await page.getByLabel("新 worktree 目录", { exact: true }).fill("/worktrees/helper");
  await page.getByLabel("新分支", { exact: true }).fill("feat/helper");
  await page.getByLabel("起始版本", { exact: true }).fill("main");
  await page.screenshot({ path: testInfo.outputPath("launch-wide.png") });
  await page.setViewportSize({ width: 600, height: 1000 });
  await page.screenshot({ path: testInfo.outputPath("launch-narrow.png") });
  await page.setViewportSize({ width: 1600, height: 1000 });
  await page.getByRole("button", { name: "启动 Agent", exact: true }).click();
  await expect(page.locator(".agent-pane")).toHaveCount(1);
  expect(state.launchRequests[0]).toMatchObject({ profile: { id: "chosen", revision: 2 }, project: { id: "project-0", revision: 1 }, directory_id: "directory-0", working_directory: "/repo-a", worktree: { path: "/worktrees/helper", branch: "feat/helper", ref: "main" } });
  expect(state.launchRequests[0].custom).toBeUndefined();
  await expect.poll(() => leaves(state.view.root)[0]?.pane.project_id).toBe("project-0");
  expect(leaves(state.view.root)[0].pane.directory_id).toBeUndefined();
  await expect(page.getByRole("navigation", { name: "Agent 列表" }).getByRole("button", { name: "打开 New acp · Runner one" })).toBeVisible();
  const context = await browser.newContext();
  const second = await context.newPage(); await mockWorkbench(second, state); await second.goto("/");
  await second.getByRole("button", { name: "Project A", exact: true }).click();
  await expect(second.getByRole("navigation", { name: "Agent 列表" }).getByRole("button", { name: "打开 New acp · Runner one" })).toBeVisible();
  await expect(second.locator(".agent-pane")).toHaveCount(1);
  expect(state.starts).toBe(1);
  await context.close();
});

test("keeps a prepared worktree after startup failure and only retries on an explicit click", async ({ page }) => {
  const state = workbenchState(); state.launchFailure = "start";
  await mockWorkbench(page, state); await page.goto("/");
  await page.getByRole("combobox", { name: "工作位置", exact: true }).selectOption("worktree");
  await page.getByLabel("新 worktree 目录", { exact: true }).fill("/worktrees/prepared");
  await page.getByLabel("新分支", { exact: true }).fill("feat/prepared");
  await page.getByRole("button", { name: "普通终端", exact: true }).click();
  await expect(page.getByRole("alert").filter({ hasText: "已创建 worktree：/worktrees/prepared" })).toBeVisible();
  await expect(page.getByRole("combobox", { name: "工作位置", exact: true })).toHaveValue("current");
  await expect(page.getByPlaceholder("开发机上的绝对路径")).toHaveValue("/worktrees/prepared");
  expect(state.starts).toBe(1);
  await expect(page.locator(".agent-pane")).toHaveCount(0);
  state.launchFailure = "";
  await page.getByRole("button", { name: "普通终端", exact: true }).click();
  await expect(page.locator(".agent-pane")).toHaveCount(1);
  expect(state.starts).toBe(2);
  expect(state.launchRequests[1].worktree).toBeUndefined();
  expect(state.launchRequests[1].working_directory).toBe("/worktrees/prepared");
});

test("attaches a confirmed Runtime after MCP configuration failure without starting again", async ({ page }) => {
  const state = workbenchState(); state.launchFailure = "mcp";
  await mockWorkbench(page, state); await page.goto("/");
  await page.getByRole("button", { name: "普通终端", exact: true }).click();
  await expect(page.locator(".agent-pane")).toHaveCount(1);
  await expect(page.getByRole("alert").filter({ hasText: "Agent 已启动" })).toBeVisible();
  expect(state.starts).toBe(1);
});

test("two lost launch responses survive refresh and query their original keys without replay", async ({ page }, testInfo) => {
  const state = workbenchState(); state.launchFailure = "lost";
  await mockWorkbench(page, state); await page.goto("/");
  for (let count = 1; count <= 2; count++) {
    await page.getByRole("button", { name: "普通终端", exact: true }).click();
    await expect.poll(() => state.starts).toBe(count);
    await expect(page.getByRole("button", { name: "普通终端", exact: true })).toBeEnabled();
  }
  const ids = state.launchRequests.map((request) => request.submission_id);
  expect(new Set(ids).size).toBe(2);
  await page.reload();
  const recovery = page.getByLabel("启动恢复记录");
  await recovery.locator("summary").click();
  await expect(recovery.getByText("接纳未确认", { exact: true })).toHaveCount(2);
  await expect(page.locator(".agent-pane")).toHaveCount(0);
  for (const id of ids) {
    const row = recovery.locator("div.border-b").filter({ has: page.locator(`code[title="${id}"]`) });
    await row.getByRole("button", { name: "查询原启动" }).click();
    await expect(row.getByText("会话已启动", { exact: true })).toBeVisible();
  }
  expect(state.launchQueries).toEqual(ids.map((submission_id) => ({ submission_id, binding: state.runners[0].binding })));
  expect(state.starts).toBe(2);
  await expect(page.locator(".agent-pane")).toHaveCount(0);
  await page.screenshot({ path: testInfo.outputPath("launch-recovery-wide.png") });
  await page.setViewportSize({ width: 600, height: 1000 });
  await page.screenshot({ path: testInfo.outputPath("launch-recovery-narrow.png") });
  await recovery.getByRole("button", { name: "打开原会话" }).first().click();
  await expect(page.locator(".agent-pane")).toHaveCount(1);
  expect(state.starts).toBe(2);
  await recovery.getByRole("button", { name: "移除本地记录" }).first().click();
  await expect(recovery.getByRole("button", { name: "查询原启动" })).toHaveCount(1);
  expect(state.runtimes.one.filter((runtime) => runtime.id.startsWith("new-"))).toHaveLength(2);
});

test("storage failure blocks launch before any request", async ({ page }) => {
  const state = workbenchState();
  await mockWorkbench(page, state); await page.goto("/");
  await page.evaluate(() => { Storage.prototype.setItem = () => { throw new Error("storage unavailable"); }; });
  await page.getByRole("button", { name: "普通终端", exact: true }).click();
  await expect(page.getByRole("alert").filter({ hasText: "storage unavailable" })).toBeVisible();
  expect(state.starts).toBe(0);
});

test("changed bindings and another login cannot recover into a replacement target", async ({ page }) => {
  const state = workbenchState(); state.launchFailure = "lost";
  await mockWorkbench(page, state); await page.goto("/");
  await page.getByRole("button", { name: "普通终端", exact: true }).click();
  await expect.poll(() => state.starts).toBe(1);
  await expect(page.getByRole("button", { name: "普通终端", exact: true })).toBeEnabled();
  const original = { ...state.runners[0].binding! };
  state.runners[0].binding!.revision++;
  await page.reload();
  const recovery = page.getByLabel("启动恢复记录");
  await recovery.locator("summary").click();
  await recovery.getByRole("button", { name: "查询原启动" }).click();
  await expect(recovery.getByRole("alert")).toContainText("环境绑定已变化");
  expect(state.launchQueries[0].binding).toEqual(original);
  await expect(recovery.getByRole("button", { name: "打开原会话" })).toHaveCount(0);
  state.accountID = "another";
  await page.reload();
  await expect(page.getByRole("heading", { name: "并行工作台" })).toBeVisible();
  await expect(page.getByLabel("启动恢复记录")).toHaveCount(0);
  expect(state.starts).toBe(1);
});
