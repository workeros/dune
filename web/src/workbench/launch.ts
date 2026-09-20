import type { AgentRuntime, Profile } from "../lib/api";

export type LaunchRequest = {
  submission_id: string;
  project?: { id: string; revision: number }; directory_id?: string;
  profile?: { id: string; revision: number }; custom?: Profile; working_directory: string;
  worktree?: { path: string; branch: string; ref?: string };
};
export type LaunchResult = { runtime?: AgentRuntime; worktree?: { path: string; branch: string } };
