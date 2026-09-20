import { test, expect } from "@playwright/test";
import { mockWorkbench, workbenchState } from "./workbench-fixture";
import type { ModelEntry } from "../src/components/acp-model";

const message = (order: number, text: string, revision = "10"): ModelEntry => ({ entry_id: `e-${order}`, order: String(order), entry_revision: revision, type: "message", content_omitted: false, context_incomplete: false, message: { role: "agent", channel: "message", status: "completed", content: [{ type: "text", text }] } });

test("model restoration, older-page tool refresh, reload and second browser are read-only", async ({ page, browser }, info) => {
 const state = workbenchState();
 const acp = state.acpStates["one-acp"];
 Object.assign(acp.conversation!, { revision: "10", head_order: "4", retained_entry_count: 4 });
 let send: (value: unknown) => void = () => {}, toolRevision = "10";
 const calls: any[] = [];
 const install = async (target: typeof page) => {
  await mockWorkbench(target, state);
  await target.routeWebSocket(/one-acp\/events/, (socket) => { send = (value) => socket.send(JSON.stringify(value)); send({ type: "acp_state", payload: acp }); });
  await target.route("**/runners/one/call?*", async (route) => {
   const body = route.request().postDataJSON(); calls.push(body);
   const reply = (value: unknown) => route.fulfill({ contentType: "application/json", body: JSON.stringify(value) });
   const tool: ModelEntry = { entry_id: "e-1", order: "1", entry_revision: toolRevision, type: "tool", content_omitted: false, context_incomplete: false, tool: { tool_call_id: "older-tool", status: toolRevision === "11" ? "completed" : "in_progress", fields: { title: "旧页中的工具", rawOutput: toolRevision === "11" ? "TOOL-FINAL" : "still working" } } };
   if (body.operation === "acp.conversation.read") return reply({ conversation: acp.conversation, entries: body.payload.cursor ? [tool, message(2, "EARLIER-CONTENT")] : [message(3, "RECENT-CONTENT"), message(4, "LATEST-CONTENT")], through_order: "4", has_more: !body.payload.cursor, next_cursor: body.payload.cursor ? undefined : "older", range_evicted: false });
   if (body.operation === "acp.conversation.get") return reply({ conversation: acp.conversation, entries: [tool], missing: [], unprocessed_entry_ids: [] });
   return route.fallback();
  });
 };
 await install(page); await page.goto("/");
 await page.getByRole("button", { name: "打开 A-ACP · Runner one", exact: true }).click();
 await expect(page.getByText("RECENT-CONTENT", { exact: true })).toBeVisible();
 await page.getByRole("button", { name: "读取更早内容" }).click();
 await expect(page.getByText("EARLIER-CONTENT", { exact: true })).toBeVisible();
 toolRevision = "11"; acp.conversation!.revision = "11";
 send({ type: "acp_conversation_changed", payload: { conversation_id: acp.conversation!.conversation_id, previous_revision: "10", revision: "11", changed_entry_ids: ["e-1"], invalidates_all: false } });
 await expect(page.locator('[data-entry-id="e-1"] summary')).toContainText("完成");
 await page.locator('[data-entry-id="e-1"] summary').click();
 await expect(page.getByText(/TOOL-FINAL/)).toBeVisible();
 await page.screenshot({ path: info.outputPath("conversation-wide.png"), fullPage: true });
 await page.setViewportSize({ width: 600, height: 900 });
 await expect(page.getByRole("button", { name: "重新读取最近内容" })).toBeVisible();
 await page.screenshot({ path: info.outputPath("conversation-narrow.png"), fullPage: true });
 await page.reload(); await expect(page.getByText("RECENT-CONTENT", { exact: true })).toBeVisible();
 const context = await browser.newContext(), second = await context.newPage();
 await install(second); await second.goto("/");
 await expect(second.getByText("LATEST-CONTENT", { exact: true })).toBeVisible();
 await context.close();
 expect(calls.some((call) => call.operation === "acp.action")).toBe(false);
 expect(calls.some((call) => call.operation === "acp.conversation.get" && call.payload.entry_ids.includes("e-1"))).toBe(true);
 expect(state.starts).toBe(0);
});

test("a held old-generation page cannot replace the selected model", async ({ page }) => {
 const state = workbenchState(), acp = state.acpStates["one-acp"];
 Object.assign(acp.conversation!, { revision: "10", head_order: "4", retained_entry_count: 4 });
 await mockWorkbench(page, state);
 let send: (value: unknown) => void = () => {}, release!: () => void, held = false;
 const barrier = new Promise<void>((resolve) => { release = resolve; });
 await page.routeWebSocket(/one-acp\/events/, (socket) => { send = (value) => socket.send(JSON.stringify(value)); send({ type: "acp_state", payload: acp }); });
 await page.route("**/runners/one/call?*", async (route) => {
  const body = route.request().postDataJSON();
  if (body.operation !== "acp.conversation.read") return route.fallback();
  const conversation = { ...acp.conversation! };
  if (body.payload.cursor) { held = true; await barrier; }
  const text = body.payload.cursor ? "OLD-PAGE-MUST-NOT-APPEAR" : conversation.conversation_id === "c2" ? "NEW-MODEL" : "FIRST-MODEL";
  await route.fulfill({ contentType: "application/json", body: JSON.stringify({ conversation, entries: [message(body.payload.cursor ? 1 : 4, text)], through_order: "4", has_more: conversation.conversation_id !== "c2" && !body.payload.cursor, next_cursor: "older", range_evicted: false }) });
 });
 await page.goto("/"); await page.getByRole("button", { name: "打开 A-ACP · Runner one", exact: true }).click();
 await expect(page.getByText("FIRST-MODEL", { exact: true })).toBeVisible();
 await page.getByRole("button", { name: "读取更早内容" }).click();
 await expect.poll(() => held).toBe(true);
 acp.revision++; acp.conversation = { ...acp.conversation!, conversation_id: "c2", revision: "1" };
 send({ type: "acp_state", payload: acp });
 release();
 await expect(page.getByText("NEW-MODEL", { exact: true })).toBeVisible();
 await expect(page.getByText("OLD-PAGE-MUST-NOT-APPEAR", { exact: true })).toHaveCount(0);
 await expect(page.getByText("FIRST-MODEL", { exact: true })).toHaveCount(0);
});
