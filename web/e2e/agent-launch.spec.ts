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
  await expect.poll(() => leaves(state.view.root)[0]?.pane.session_record_id).toBe("record-1");
  expect(leaves(state.view.root)[0].pane.project_id).toBe("project-0");
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

test("attaches a confirmed Runtime after recovery-index failure without starting again", async ({ page }) => {
  const state = workbenchState(); state.launchFailure = "index";
  await mockWorkbench(page, state); await page.goto("/");
  await page.getByRole("button", { name: "普通终端", exact: true }).click();
  await expect(page.locator(".agent-pane")).toHaveCount(1);
  await expect(page.getByRole("alert").filter({ hasText: "Agent 已启动" })).toBeVisible();
  expect(state.starts).toBe(1);
});
