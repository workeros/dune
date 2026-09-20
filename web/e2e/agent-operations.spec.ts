import { test, expect } from "@playwright/test";
import { mockWorkbench, workbenchState, workbenchACPState } from "./workbench-fixture";

function operationReceipt(submissionID: string, ref: string) {
 return { submission_id: submissionID, admission: "accepted", operation_ref: ref, target: { owner_id: "owner", runner_id: "one", fabric_id: "attached", machine_id: "machine-one", binding_revision: 1, runtime_id: "one-acp", runtime_incarnation: "boot-one", runtime_generation: 1 } };
}

test("stop keeps its original receipt query after the Runtime disappears and the page reloads", async ({ page }, testInfo) => {
 const state = workbenchState(); await mockWorkbench(page, state);
 let submitted: any, saved: any, sends = 0;
 await page.route("**/api/v1/agents/submit", async (route) => {
  sends++; submitted = route.request().postDataJSON();
  saved = await page.evaluate((id) => JSON.parse(sessionStorage.getItem('dune.acpSubmissions:"owner"') ?? "[]").find((item: any) => item.submission_id === id), submitted.submission_id);
  expect(saved).toMatchObject({ action: "stop", agent_ref: "ref-one-acp", runtime: { id: "one-acp", incarnation: "boot-one", generation: 1 } });
  expect(submitted).toEqual({ submission_id: saved.submission_id, agent_ref: saved.agent_ref, action: "stop" });
  state.runtimes.one = state.runtimes.one.filter((runtime) => runtime.id !== "one-acp");
  await route.abort("failed");
 });
 await page.route("**/api/v1/agents/submission", async (route) => {
  expect(route.request().postDataJSON()).toEqual({ submission_id: submitted.submission_id, agent_ref: submitted.agent_ref });
  await route.fulfill({ contentType: "application/json", body: JSON.stringify({ submission_id: submitted.submission_id, admission: "accepted", operation_ref: "original-stop", stage: "stopped", target: { owner_id: "owner", ...saved.binding, binding_revision: saved.binding.revision, runtime_id: saved.runtime.id, runtime_incarnation: saved.runtime.incarnation, runtime_generation: saved.runtime.generation } }) });
 });
 await page.goto("/"); await page.getByRole("button", { name: "打开 A-ACP · Runner one", exact: true }).click();
 await page.getByRole("button", { name: "停止 A-ACP", exact: true }).click();
 await page.getByRole("button", { name: "确认停止", exact: true }).click();
 await expect.poll(() => sends).toBe(1);
 await page.reload();
 const records = page.getByLabel("历史提交查询", { exact: true });
 await records.locator("summary").click();
 await records.getByRole("button", { name: "查询原提交", exact: true }).click();
 await expect(records.getByRole("status")).toHaveText("已停止");
 expect(sends).toBe(1); expect(state.starts).toBe(0);
 await page.screenshot({ path: testInfo.outputPath("stop-recovery-without-runtime.png"), fullPage: true });
});

test("forget saves the lost Runtime before sending and keeps cleanup progress after reload", async ({ page }, testInfo) => {
 const state = workbenchState();
 const original = state.runtimes.one.find((runtime) => runtime.id === "one-acp")!;
 original.state = "lost"; original.availability = "lost";
 await mockWorkbench(page, state);
 let submitted: any, saved: any, sends = 0, completed = false;
 await page.route("**/api/v1/agents/submit", async (route) => {
  sends++; submitted = route.request().postDataJSON();
  saved = await page.evaluate((id) => JSON.parse(sessionStorage.getItem('dune.acpSubmissions:"owner"') ?? "[]").find((item: any) => item.submission_id === id), submitted.submission_id);
  expect(saved).toMatchObject({ action: "forget", agent_ref: "ref-one-acp", runtime: { id: "one-acp", incarnation: "boot-one", generation: 1 } });
  expect(submitted).toEqual({ submission_id: saved.submission_id, agent_ref: saved.agent_ref, action: "forget" });
  state.runtimes.one = state.runtimes.one.filter((runtime) => runtime.id !== "one-acp");
  await route.abort("failed");
 });
 await page.route("**/api/v1/agents/submission", async (route) => {
  expect(route.request().postDataJSON()).toEqual({ submission_id: submitted.submission_id, agent_ref: submitted.agent_ref });
  await route.fulfill({ contentType: "application/json", body: JSON.stringify({ submission_id: submitted.submission_id, admission: "accepted", operation_ref: "original-cleanup", stage: completed ? "completed" : "cleaning", error_code: completed ? "" : "CLEANUP_UNCONFIRMED", cleanup: { confirmed: completed ? ["host", "ipc", "runtime_directory"] : ["host"], remaining: completed ? [] : ["ipc", "runtime_directory"] }, target: { owner_id: "owner", ...saved.binding, binding_revision: saved.binding.revision, runtime_id: saved.runtime.id, runtime_incarnation: saved.runtime.incarnation, runtime_generation: saved.runtime.generation } }) });
 });
 await page.goto("/"); await page.getByRole("button", { name: "打开 A-ACP · Runner one", exact: true }).click();
 await page.getByRole("button", { name: "删除 A-ACP", exact: true }).click();
 await page.getByRole("button", { name: "确认删除", exact: true }).click();
 await expect.poll(() => sends).toBe(1);
 await page.reload();
 const records = page.getByLabel("历史提交查询", { exact: true });
 await records.locator("summary").click();
 await records.getByRole("button", { name: "查询原提交", exact: true }).click();
 await expect(records.getByRole("status")).toHaveText("清理尚未完成");
 await expect(records.getByText("已确认 1/3 个清理步骤", { exact: true })).toBeVisible();
 completed = true;
 await records.getByRole("button", { name: "查询原提交", exact: true }).click();
 await expect(records.getByRole("status")).toHaveText("清理已完成");
 expect(sends).toBe(1); expect(state.starts).toBe(0); expect(state.connections.some((path) => path.includes("one-acp"))).toBe(false);
 await page.screenshot({ path: testInfo.outputPath("forget-recovery-without-runtime.png"), fullPage: true });
});

test("busy ACP queues a task and reads only that operation across idle and output gaps", async ({ page }, testInfo) => {
 const state = workbenchState(); await mockWorkbench(page, state);
 let send: (value: unknown) => void = () => undefined;
 const acp = { ...workbenchACPState("one-acp"), ready: true, revision: 1, busy: "prompt", pending: 0, session_id: "native", cwd: "/workspace", can_load: true, can_list: false, permissions: [] };
 state.acpStates["one-acp"] = acp;
 await page.routeWebSocket(/one-acp\/events/, (socket) => { send = (value) => socket.send(JSON.stringify(value)); send({ type: "acp_state", payload: acp }); });
 const submissions: any[] = [], reads: any[] = [], waits: any[] = [];
 let operationState = "pending", expired = false;
 const operation = () => ({ operation_ref: "operation-b", state: operationState, stop_reason: operationState === "completed" ? "end_turn" : undefined });
 await page.route("**/api/v1/agents/*", async (route) => {
  const path = new URL(route.request().url()).pathname, body = route.request().postDataJSON();
  const reply = (value: unknown, status = 200) => route.fulfill({ status, contentType: "application/json", body: JSON.stringify(value) });
  if (path.endsWith("/prompt")) { submissions.push(body); return reply({ ...operation(), submission: operationReceipt(body.submission_id, "operation-b") }); }
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
 expect(submissions).toEqual([{ submission_id: expect.any(String), agent_ref: "ref-one-acp", text: "任务 B", expected_conversation_id: "conversation-one-acp", wait_ms: 0 }]);
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
  const ref = native ? `operation-${calls.length}` : "operation-unknown";
  const operation = { operation_ref: ref, state: native ? "completed" : "unknown", submission: operationReceipt(body.submission_id, ref) };
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
 expect(calls.map((call) => call.body)).toEqual([{ submission_id: expect.any(String), agent_ref: "ref-one-acp", action: "new", wait_ms: 0 }, { submission_id: expect.any(String), agent_ref: "ref-one-acp", action: "load", session_id: "saved-native", wait_ms: 0 }, { submission_id: expect.any(String), agent_ref: "ref-one-acp", text: "do once", expected_conversation_id: "conversation-one-acp", wait_ms: 0 }]);
 expect(state.calls.filter((call) => call.operation === "acp.action")).toHaveLength(0);
});

test("ACP controls save caller identity before sending and select the exact prompt", async ({ page }) => {
 const state = workbenchState(); await mockWorkbench(page, state);
 state.acpStates["one-acp"] = { ...workbenchACPState("one-acp"), ready: true, busy: "prompt", operation_ref: "original-prompt", permissions: [{ id: "original-permission", params: { toolCall: { title: "测试工具" }, options: [{ optionId: "allow", name: "允许一次", kind: "allow_once" }] } }] };
 const calls: any[] = [];
 await page.route("**/api/v1/agents/submit", async (route) => {
  const body = route.request().postDataJSON(); calls.push(body);
  const saved = await page.evaluate((id) => JSON.parse(sessionStorage.getItem('dune.acpSubmissions:"owner"') ?? "[]").find((item: any) => item.submission_id === id), body.submission_id);
  expect(saved).toMatchObject({ submission_id: body.submission_id, agent_ref: body.agent_ref, prefix: "/api/v1" });
  await route.fulfill({ contentType: "application/json", body: JSON.stringify({ submission_id: body.submission_id, admission: "accepted", stage: "written", operation_ref: "control", target: { owner_id: "owner", ...saved.binding, binding_revision: saved.binding.revision, runtime_id: saved.runtime.id, runtime_incarnation: saved.runtime.incarnation, runtime_generation: saved.runtime.generation } }) });
 });
 await page.goto("/"); await page.getByRole("button", { name: "打开 A-ACP · Runner one", exact: true }).click();
 await page.getByRole("button", { name: "允许一次", exact: true }).click();
 await expect.poll(() => calls.length).toBe(1);
 await page.getByRole("button", { name: "取消任务", exact: true }).click();
 await expect.poll(() => calls.length).toBe(2);
 expect(calls[0]).toMatchObject({ agent_ref: "ref-one-acp", action: "permission", permission_id: "original-permission", option_id: "allow" });
 expect(calls[1]).toMatchObject({ agent_ref: "ref-one-acp", action: "cancel", operation_ref: "original-prompt" });
 expect(calls[0].submission_id).not.toBe(calls[1].submission_id);
 expect(state.calls.some((call) => call.operation === "acp.action")).toBe(false);
});

test("lost first response survives refresh and queries the original key without replay", async ({ page }, testInfo) => {
 const state = workbenchState(); await mockWorkbench(page, state);
 let submitted: any, saved: any, sends = 0, queries = 0;
 const task = "sensitive prompt never persisted";
 await page.route("**/api/v1/agents/prompt", async (route) => {
  sends++; submitted = route.request().postDataJSON();
  saved = await page.evaluate((id) => JSON.parse(sessionStorage.getItem('dune.acpSubmissions:"owner"') ?? "[]").find((item: any) => item.submission_id === id), submitted.submission_id);
  expect(saved).toBeTruthy();
  expect(JSON.stringify(saved)).not.toContain(task);
  await route.abort("failed");
 });
 await page.route("**/api/v1/agents/submission", async (route) => {
  queries++;
  expect(route.request().postDataJSON()).toEqual({ submission_id: submitted.submission_id, agent_ref: submitted.agent_ref });
  await route.fulfill({ contentType: "application/json", body: JSON.stringify({ submission_id: submitted.submission_id, admission: queries === 1 ? "unknown" : "accepted", operation_ref: queries === 1 ? undefined : "original-sdk-operation", target: { owner_id: "owner", ...saved.binding, binding_revision: saved.binding.revision, runtime_id: saved.runtime.id, runtime_incarnation: saved.runtime.incarnation, runtime_generation: saved.runtime.generation } }) });
 });
 await page.route(/\/runners\/[^/]+\/call\?/, async (route) => {
  const body = route.request().postDataJSON();
  if (body.operation !== "agent.operation.wait") return route.fallback();
  expect(body.payload.operation_ref).toBe("original-sdk-operation");
  expect(body.runtime).toEqual(saved.runtime);
  await route.fulfill({ contentType: "application/json", body: JSON.stringify({ operation_ref: "original-sdk-operation", state: "completed" }) });
 });
 await page.goto("/"); await page.getByRole("button", { name: "打开 A-ACP · Runner one", exact: true }).click();
 await page.getByLabel("发送给 Agent 的任务").fill(task);
 await page.getByRole("button", { name: "发送", exact: true }).click();
 await expect(page.getByText(/已保存提交标识/)).toBeVisible();
 await page.reload();
 await page.getByRole("button", { name: "打开 A-ACP · Runner one", exact: true }).click();
 const records = page.getByLabel("提交恢复记录");
 await records.locator("summary").click();
 await records.getByRole("button", { name: "查询原提交", exact: true }).click();
 await expect(records.getByRole("status")).toHaveText("接纳未确认");
 await expect(records.getByRole("button", { name: "查询原提交", exact: true })).toBeEnabled();
 await records.getByRole("button", { name: "查询原提交", exact: true }).click();
 await expect(records.getByRole("status")).toHaveText("操作已完成");
 expect(sends).toBe(1); expect(queries).toBe(2);
 await page.screenshot({ path: testInfo.outputPath("submission-recovery.png"), fullPage: true });
 state.accountID = "another";
 await page.reload();
 await expect(page.getByRole("heading", { name: "并行工作台" })).toBeVisible();
 await expect(page.getByLabel("历史提交查询", { exact: true })).toHaveCount(0);
 await expect(page.getByLabel("提交恢复记录", { exact: true })).toHaveCount(0);
 expect(sends).toBe(1); expect(queries).toBe(2);
});

test("an operation with a mismatched receipt is not followed or replayed", async ({ page }) => {
 const state = workbenchState(); await mockWorkbench(page, state);
 let sends = 0, waits = 0;
 await page.route("**/api/v1/agents/prompt", async (route) => {
  sends++;
  const body = route.request().postDataJSON();
  await route.fulfill({ contentType: "application/json", body: JSON.stringify({ operation_ref: "unrelated-operation", state: "running", submission: operationReceipt(body.submission_id, "original-operation") }) });
 });
 await page.route("**/api/v1/agents/wait", async (route) => { waits++; await route.abort(); });
 await page.goto("/"); await page.getByRole("button", { name: "打开 A-ACP · Runner one", exact: true }).click();
 await page.getByLabel("发送给 Agent 的任务").fill("do once");
 await page.getByRole("button", { name: "发送", exact: true }).click();
 await expect(page.getByRole("alert").filter({ hasText: "操作引用与原提交回执不一致" })).toBeVisible();
 await expect(page.getByLabel("任务 1", { exact: true })).toHaveCount(0);
 await expect(page.getByLabel("发送给 Agent 的任务")).toHaveValue("do once");
 expect(sends).toBe(1); expect(waits).toBe(0);
});
