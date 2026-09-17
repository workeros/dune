import { test, expect } from "@playwright/test";
import { mockWorkbench, workbenchState } from "./workbench-fixture";

test("busy ACP queues a task and reads only that operation across idle and output gaps", async ({ page }, testInfo) => {
 const state = workbenchState(); await mockWorkbench(page, state);
 let send: (value: unknown) => void = () => undefined;
 const acp = { ready: true, revision: 1, busy: "prompt", pending: 0, session_id: "native", cwd: "/workspace", can_load: true, can_list: false, permissions: [] };
 await page.routeWebSocket(/one-acp\/events/, (socket) => { send = (value) => socket.send(JSON.stringify(value)); send({ type: "acp_state", payload: acp }); });
 const submissions: any[] = [], reads: any[] = [], waits: any[] = [];
 let operationState = "pending", expired = false;
 const operation = () => ({ operation_ref: "operation-b", state: operationState, stop_reason: operationState === "completed" ? "end_turn" : undefined });
 await page.route("**/api/v1/agents/*", async (route) => {
  const path = new URL(route.request().url()).pathname, body = route.request().postDataJSON();
  const reply = (value: unknown, status = 200) => route.fulfill({ status, contentType: "application/json", body: JSON.stringify(value) });
  if (path.endsWith("/prompt")) { submissions.push(body); return reply(operation()); }
  if (path.endsWith("/wait")) { waits.push(body); return reply({ operation: operation() }); }
  if (path.endsWith("/read")) {
   reads.push(body);
   if (expired) return reply({ code: "OPERATION_EXPIRED", error: "expired" }, 410);
   return reply({ operation: { ...operation(), position: operationState === "completed" && body.position === 0 ? 2 : body.position, next_position: operationState === "completed" ? 3 : 0, incomplete: operationState === "completed", output: operationState === "completed" && body.position < 3 ? [{ update: { sessionUpdate: "agent_message_chunk", content: { type: "text", text: "只属于任务 B 的结果" } } }] : [] } });
  }
  return route.fallback();
 });
 await page.goto("/"); await page.getByRole("button", { name: "打开 A-ACP · Runner one", exact: true }).click();
 await page.getByLabel("发送给 Agent 的任务").fill("任务 B");
 await page.getByRole("button", { name: "加入队列", exact: true }).click();
 await expect(page.getByLabel("任务 1", { exact: true })).toContainText("排队中");
 expect(submissions).toEqual([{ agent_ref: "ref-one-acp", text: "任务 B", wait_ms: 0 }]);
 acp.busy = ""; acp.revision++; send({ type: "acp_state", payload: acp });
 await expect.poll(() => waits.length).toBeGreaterThan(0);
 await expect(page.getByLabel("任务 1", { exact: true })).toContainText("排队中");
 await page.getByRole("button", { name: "查看本次输出" }).click();
 const dialog = page.getByRole("dialog");
 await expect(dialog.getByText("本次操作尚无可读输出。")).toBeVisible();
 expect(reads[0]).toEqual({ operation_ref: "operation-b", position: 0, limit: 64 });
 operationState = "completed";
 await dialog.getByRole("button", { name: "读取后续输出" }).click();
 await expect(dialog.getByText("只属于任务 B 的结果", { exact: true })).toBeVisible();
 await expect(dialog.getByRole("alert")).toContainText("本次输出不完整");
 await page.screenshot({ path: testInfo.outputPath("operation-output-wide.png"), fullPage: true });
 await page.setViewportSize({ width: 600, height: 900 });
 await expect(dialog.getByRole("button", { name: "读取后续输出" })).toBeInViewport();
 await page.screenshot({ path: testInfo.outputPath("operation-output-narrow.png"), fullPage: true });
 expired = true;
 await dialog.getByRole("button", { name: "读取后续输出" }).click();
 await expect(dialog.getByText("本次操作引用已失效，无法查询。不能据此判断任务没有执行。")).toBeVisible();
 expect(reads.at(-1).position).toBe(3);
 expect(waits.every((body) => body.operation_ref === "operation-b" && !body.agent_ref)).toBe(true);
 expect(state.calls.some((call) => call.operation === "acp.action" && call.payload.action === "prompt")).toBe(false);
 expect(submissions).toHaveLength(1);
});

test("ACP native actions use shared references and unknown prompt receipts are retained without replay", async ({ page }) => {
 const state = workbenchState(); await mockWorkbench(page, state);
 const calls: { path: string; body: any }[] = [];
 await page.route("**/api/v1/agents/*", async (route) => {
  const path = new URL(route.request().url()).pathname, body = route.request().postDataJSON();
  calls.push({ path, body });
  const native = path.endsWith("/open-session");
  const operation = { operation_ref: native ? `operation-${calls.length}` : "operation-unknown", state: native ? "completed" : "unknown" };
  await route.fulfill({ status: native ? 200 : 503, contentType: "application/json", body: JSON.stringify(native ? operation : { code: "RESULT_UNKNOWN", error: "任务回执未知", result: operation }) });
 });
 await page.goto("/"); await page.getByRole("button", { name: "打开 A-ACP · Runner one", exact: true }).click();
 await page.getByRole("button", { name: "新建对话", exact: true }).click();
 await expect(page.getByLabel("新建对话 1", { exact: true })).toContainText("已完成");
 await page.getByLabel("ACP 会话 ID").fill("saved-native");
 await page.getByRole("button", { name: "从 Agent 加载历史", exact: true }).click();
 await expect(page.getByLabel("加载对话 2", { exact: true })).toContainText("已完成");
 await page.getByLabel("发送给 Agent 的任务").fill("do once");
 await page.getByRole("button", { name: "发送", exact: true }).click();
 await expect(page.getByLabel("任务 3", { exact: true })).toContainText("结果未知");
 await expect(page.getByText(/已保留本次操作，可以继续查询/)).toBeVisible();
 await page.waitForTimeout(1200);
 expect(calls.map((call) => call.body)).toEqual([{ agent_ref: "ref-one-acp", action: "new", wait_ms: 0 }, { agent_ref: "ref-one-acp", action: "load", session_id: "saved-native", wait_ms: 0 }, { agent_ref: "ref-one-acp", text: "do once", wait_ms: 0 }]);
 expect(state.calls.filter((call) => call.operation === "acp.action")).toHaveLength(0);
});
