import { expect, test } from "@playwright/test";
import { mockWorkbench, workbenchState } from "./workbench-fixture";
import { leaves, targetFor } from "../src/workbench/model";

for (const adapter of ["acp", "pty"] as const) {
  test(`${adapter} reconnects while alive and remains ended after exit and reload`, async ({ page }) => {
    const state = workbenchState(), original = state.runtimes.one.find((runtime) => runtime.adapter === adapter)!, other = state.runtimes.two[0];
    state.view = { id: "main", revision: 1, root: { id: "split", direction: "horizontal", ratio: 0.6, children: [{ id: "old-pane", pane: { target: targetFor(state.runners[0].binding!, original) } }, { id: "other-pane", pane: { target: targetFor(state.runners[1].binding!, other) } }] }, focus_pane: "old-pane", review_pane: "old-pane" };
    const recoveryRequests: string[] = [];
    page.on("request", (request) => { if (request.url().includes("/agent-sessions")) recoveryRequests.push(request.url()); });
    await mockWorkbench(page, state); await page.goto("/");
    await expect.poll(() => state.connections.filter((path) => path.includes(original.id)).length).toBe(1);
    await page.reload();
    await expect.poll(() => state.connections.filter((path) => path.includes(original.id)).length).toBe(2);
    const otherConnections = state.connections.filter((path) => path.includes(other.id)).length;
    state.runtimes.one = state.runtimes.one.filter((runtime) => runtime.id !== original.id);
    await page.getByRole("button", { name: "刷新", exact: true }).click();
    await expect(page.getByText("原会话已结束，可以新建 Agent。", { exact: true })).toBeVisible();
    expect(state.connections.filter((path) => path.includes(other.id))).toHaveLength(otherConnections);
    await page.reload();
    await expect(page.getByText("原会话已结束，可以新建 Agent。", { exact: true })).toBeVisible();
    await expect(page.getByRole("button", { name: "继续会话", exact: true })).toHaveCount(0);
    expect(state.connections.filter((path) => path.includes(original.id))).toHaveLength(2);
    expect(leaves(state.view.root)).toHaveLength(2);
    expect(state.view.review_pane).toBe("old-pane");
    expect(recoveryRequests).toEqual([]); expect(state.starts).toBe(0);
  });
}
