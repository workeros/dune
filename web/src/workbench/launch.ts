import type { AgentRuntime, Binding, Profile } from "../lib/api";

export type AgentSession = {
  id: string; revision: number; selected: boolean; binding: Binding; project_id?: string; directory_id?: string;
  working_directory: string; adapter: "pty" | "acp"; agent_type: string;
  status: "pending_capture" | "available" | "unavailable" | "unknown"; reason?: string;
  native?: { id: string; cwd: string; resume_supported: boolean };
  attempt?: { id: string; kind: "start" | "resume"; state: "starting" | "capturing" | "ready" | "failed" | "unknown"; base_revision: number };
  last_runtime?: Pick<AgentRuntime, "id" | "incarnation" | "generation" | "adapter">;
};
export type LaunchRequest = {
  project?: { id: string; revision: number }; directory_id?: string;
  profile?: { id: string; revision: number }; custom?: Profile; working_directory: string;
  worktree?: { path: string; branch: string; ref?: string };
};
export type LaunchResult = { runtime?: AgentRuntime; session?: AgentSession; worktree?: { path: string; branch: string } };

export type ResumeResult = { runtime?: AgentRuntime; session?: AgentSession; operation?: { operation_ref: string; state: string } };
