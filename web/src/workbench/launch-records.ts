import type { Binding } from "../lib/api";
import type { LaunchIdentity, LaunchReceipt } from "./launch";

export type LaunchScope = { accountID: string; prefix: string };
export type SavedLaunch = { submission_id: string; binding: Binding; created_at: string; admission?: LaunchReceipt["admission"]; stage?: string };
export const launchesChanged = "dune-launches-changed";
const capacity = 64, byteLimit = 256 * 1024;
const admissions = new Set(["unknown", "accepted", "not_accepted", "expired"]);

function storageKey(scope: LaunchScope) {
  if (!scope.accountID || !scope.prefix) throw new Error("无法确认启动记录所属账号。");
  return `dune.launches:${JSON.stringify([scope.accountID, scope.prefix])}`;
}
const validString = (value: unknown): value is string => typeof value === "string" && value.length > 0 && value.length <= 256;
function validSaved(value: unknown): value is SavedLaunch {
  if (!value || typeof value !== "object") return false;
  const item = value as SavedLaunch, binding = item.binding;
  return validString(item.submission_id) && validString(item.created_at) && Number.isFinite(Date.parse(item.created_at)) && !!binding && validString(binding.runner_id) && validString(binding.fabric_id) && validString(binding.machine_id) && Number.isSafeInteger(binding.revision) && binding.revision > 0 && (item.admission === undefined || admissions.has(item.admission)) && (item.stage === undefined || validString(item.stage));
}
export function readLaunches(scope: LaunchScope): SavedLaunch[] {
  const raw = sessionStorage.getItem(storageKey(scope));
  if (!raw) return [];
  if (raw.length > byteLimit) throw new Error("本地启动记录超过容量，未覆盖原记录。");
  const items: unknown = JSON.parse(raw);
  if (!Array.isArray(items) || items.length > capacity || items.some((item) => !validSaved(item)) || new Set(items.map((item) => item.submission_id)).size !== items.length) throw new Error("本地启动记录损坏，未覆盖原记录。");
  return items;
}
function writeLaunches(scope: LaunchScope, items: SavedLaunch[]) {
  const raw = JSON.stringify(items);
  if (items.length > capacity || raw.length > byteLimit) throw new Error("本地启动记录已满，请先移除不再需要的记录。");
  sessionStorage.setItem(storageKey(scope), raw);
  window.dispatchEvent(new Event(launchesChanged));
}
// Persist only selectors before the first send. Unknown records are never evicted
// to make room for another launch, and storage failure prevents that send.
export function saveLaunch(scope: LaunchScope, binding: Binding, submissionID: string) {
  const items = readLaunches(scope);
  const saved = { submission_id: submissionID, binding: { ...binding }, created_at: new Date().toISOString() };
  if (!validSaved(saved) || items.some((item) => item.submission_id === submissionID)) throw new Error("启动提交身份无效。");
  writeLaunches(scope, [...items, saved]);
}
export function matchesLaunch(scope: LaunchScope, saved: SavedLaunch, value: LaunchIdentity): boolean {
  const target = value?.target, binding = saved.binding;
  return value?.submission_id === saved.submission_id && target?.owner_id === scope.accountID && target.runner_id === binding.runner_id && target.fabric_id === binding.fabric_id && target.machine_id === binding.machine_id && target.binding_revision === binding.revision && !target.runtime_id && !target.runtime_incarnation && !target.runtime_generation;
}
export function recordLaunchReceipt(scope: LaunchScope, id: string, receipt: LaunchReceipt) {
  const items = readLaunches(scope), saved = items.find((item) => item.submission_id === id);
  if (!saved || !matchesLaunch(scope, saved, receipt) || !admissions.has(receipt.admission) || receipt.stage !== undefined && !validString(receipt.stage)) throw new Error("启动回执与原提交身份不一致，请查询原提交。");
  writeLaunches(scope, items.map((item) => item === saved ? { ...item, admission: receipt.admission, stage: receipt.stage } : item));
}
export function removeLaunch(scope: LaunchScope, id: string) {
  writeLaunches(scope, readLaunches(scope).filter((item) => item.submission_id !== id));
}
