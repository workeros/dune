import type { AgentRuntime, Profile } from "../lib/api";

export type LaunchRequest = {
  submission_id: string;
  project?: { id: string; revision: number }; directory_id?: string;
  profile?: { id: string; revision: number }; custom?: Profile; working_directory: string;
  worktree?: { path: string; branch: string; ref?: string };
};
export type LaunchIdentity = { submission_id: string; target: { owner_id: string; runner_id: string; fabric_id: string; machine_id: string; binding_revision: number; runtime_id?: string; runtime_incarnation?: string; runtime_generation?: number } };
export type LaunchReceipt = LaunchIdentity & { admission: "unknown" | "accepted" | "not_accepted" | "expired"; stage?: string; error_code?: string; runtime?: AgentRuntime; worktree?: { path: string; branch: string } };
export type LaunchResult = LaunchIdentity & { submission?: LaunchReceipt; runtime?: AgentRuntime; worktree?: { path: string; branch: string } };
