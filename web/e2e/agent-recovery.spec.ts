import { expect, test } from "@playwright/test";
import { mockWorkbench, workbenchState } from "./workbench-fixture";
import { leaves, targetFor } from "../src/workbench/model";

function recoveryState(adapter: "acp" | "pty" = "acp") {
  const state = workbenchState(), original = state.runtimes.one.find((runtime) => runtime.adapter === adapter)!, other = state.runtimes.two[0];
  state.runtimes.one = state.runtimes.one.filter((runtime) => runtime !== original);
  state.sessions.push({ id: "saved-native", revision: 3, selected: true, binding: state.runners[0].binding!, project_id: "project-0", directory_id: "directory-0", adapter, working_directory: "/repo-a", agent_type: "fixture", status: "available", native: { id: "native-original", cwd: "/repo-a", resume_supported: true }, attempt: { id: "initial", kind: "start", state: "ready", base_revision: 0 }, last_runtime: original });
  state.view = { id: "main", revision: 1, root: { id: "split", direction: "horizontal", ratio: 0.6, children: [{ id: "old-pane", pane: { target: targetFor(state.runners[0].binding!, original), session_record_id: "saved-native", project_id: "project-0" } }, { id: "other-pane", pane: { target: targetFor(state.runners[1].binding!, other) } }] }, focus_pane: "old-pane", review_pane: "old-pane" };
  return state;
}

test("native recovery is explicit and replaces only the selected pane", async ({ page }, testInfo) => {
  const state = recoveryState(); await mockWorkbench(page, state); await page.goto("/");
  await expect(page.getByRole("button", { name: "继续会话", exact: true })).toBeEnabled();
  expect(state.resumeRequests).toHaveLength(0);
  await expect.poll(() => state.connections.filter((path) => path.includes("two-pty")).length).toBe(1);
  await page.screenshot({ path: testInfo.outputPath("recovery-wide.png") });
  await page.setViewportSize({ width: 700, height: 950 });
  await page.screenshot({ path: testInfo.outputPath("recovery-narrow.png") });
  await page.getByRole("button", { name: "继续会话", exact: true }).click();
  await expect.poll(() => leaves(state.view.root).find((leaf) => leaf.id === "old-pane")?.pane.target.runtime.id).toBe("resumed-acp");
  await expect.poll(() => state.connections.filter((path) => path.includes("resumed-acp")).length).toBe(1);
  expect(state.connections.filter((path) => path.includes("two-pty"))).toHaveLength(1);
  expect(state.view.review_pane).toBe("old-pane"); expect(leaves(state.view.root)).toHaveLength(2);
  expect(state.resumeRequests).toEqual([{ id: "saved-native", revision: 3 }]);
  await page.reload();
  await expect(page.getByRole("button", { name: "继续会话", exact: true })).toHaveCount(0);
  expect(state.resumeRequests).toHaveLength(1); expect(state.starts).toBe(0);
});

test("PTY recovery opens the terminal while native confirmation is pending", async ({ page }, testInfo) => {
  const state = recoveryState("pty"); state.resumeOutcome = "pending";
  await mockWorkbench(page, state); await page.goto("/");
  await page.getByRole("button", { name: "继续会话", exact: true }).click();
  await expect.poll(() => leaves(state.view.root).find((leaf) => leaf.id === "old-pane")?.pane.target.runtime.id).toBe("resumed-pty");
  await expect.poll(() => state.connections.filter((path) => path.includes("resumed-pty")).length).toBe(1);
  await expect(page.getByText("正在恢复原生会话，等待确认。", { exact: true })).toBeVisible();
  await page.screenshot({ path: testInfo.outputPath("pty-recovery-pending.png") });
  state.sessions[0].status = "available"; state.sessions[0].attempt!.state = "ready"; state.sessions[0].revision++;
  await page.getByRole("button", { name: "检查恢复结果", exact: true }).click();
  await expect(page.getByText("正在恢复原生会话，等待确认。", { exact: true })).toHaveCount(0);
  expect(state.connections.filter((path) => path.includes("resumed-pty"))).toHaveLength(1);
  expect(state.connections.filter((path) => path.includes("two-pty"))).toHaveLength(1);
  expect(state.resumeRequests).toEqual([{ id: "saved-native", revision: 3 }]);
});

test("unknown recovery results remain visible and never replay on refresh", async ({ page }) => {
  const state = recoveryState(); state.resumeOutcome = "unknown";
  await mockWorkbench(page, state); await page.goto("/");
  await page.getByRole("button", { name: "继续会话", exact: true }).click();
  await expect(page.getByText("恢复结果尚未确认，请检查结果后再继续。", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "检查恢复结果", exact: true }).click();
  await expect(page.getByRole("button", { name: "继续会话", exact: true })).toHaveCount(0);
  expect(state.resumeRequests).toHaveLength(1);
  expect(leaves(state.view.root).find((leaf) => leaf.id === "old-pane")?.pane.target.runtime.id).toBe("one-acp");
});

test("a pending recovery follows the confirmed shared attempt without submitting again", async ({ page }) => {
  const state = recoveryState(); state.resumeOutcome = "pending";
  await mockWorkbench(page, state); await page.goto("/");
  await page.getByRole("button", { name: "继续会话", exact: true }).click();
  await expect(page.getByText("正在恢复原生会话，等待确认。", { exact: true })).toBeVisible();
  const runtime = { ...state.sessions[0].last_runtime!, id: "resumed-acp", incarnation: "resumed-boot", state: "running", title: "Restored ACP", working_directory: "/repo-a" };
  state.runtimes.one.push(runtime); state.sessions[0].last_runtime = runtime; state.sessions[0].status = "available"; state.sessions[0].attempt!.state = "ready"; state.sessions[0].revision++;
  await page.getByRole("button", { name: "检查恢复结果", exact: true }).click();
  await expect.poll(() => leaves(state.view.root).find((leaf) => leaf.id === "old-pane")?.pane.target.runtime.id).toBe("resumed-acp");
  expect(state.resumeRequests).toHaveLength(1);
});
