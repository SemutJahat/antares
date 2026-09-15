/**
 * Content Creator API client and helpers.
 *
 * Types mirror the JSON contract emitted by the Go `internal/creator` service
 * and exposed by the Main-owned `/api/content-creator/*` HTTP routes. Keep
 * these in sync with `internal/creator/types.go`; changes here without a
 * matching backend change will surface as `undefined` at runtime, not as a
 * type error.
 */

import { authedUrl, get, post, put } from "./api";

export type PublishMode = "draft" | "auto";
export type Stage = "research" | "plan" | "produce" | "publish" | "full";
export type ReferenceKind = "character" | "setting" | "style" | "prop";
export type ShotContinuity = "cut" | "continue";
export type PublicationStatus =
  "not_started" | "uploading" | "published" | "blocked";

export interface Reference {
  id: string;
  kind: ReferenceKind;
  name: string;
  prompt: string;
  path: string;
  status: string;
  error: string;
}

export interface Shot {
  id: string;
  prompt: string;
  reference_ids: string[];
  duration_seconds: number;
  continuity: ShotContinuity;
  notes: string;
  keyframe_path: string;
  video_job_id: string;
  video_path: string;
  last_frame_path: string;
  status: string;
  error: string;
  progress: number;
}

export interface Trend {
  url: string;
  title: string;
  platform: string;
  observed_at: string;
  notes: string;
  metrics: Record<string, number>;
}

export interface Idea {
  id: string;
  title: string;
  hook: string;
  rationale: string;
  selected: boolean;
}

export interface Publication {
  status: PublicationStatus;
  post_url: string;
  error: string;
  account_id: string;
  artifact_hash: string;
}

export interface Project {
  id: string;
  title: string;
  brief: string;
  platform: string;
  account_id: string;
  style: string;
  size: string;
  target_seconds: number;
  caption: string;
  revision: number;
  publish_mode: PublishMode;
  status: string;
  error: string;
  created_at: string;
  updated_at: string;
  run_session_id: string;
  run_stage: Stage | "";
  run_status: string;
  last_run_error: string;
  references: Reference[];
  shots: Shot[];
  research: Trend[];
  ideas: Idea[];
  final_path: string;
  publication: Publication;
}

export interface ProviderSettings {
  enabled: boolean;
  provider: string;
  model: string;
  base_url: string;
  size: string;
  seconds?: number;
  has_key: boolean;
}

export interface CreatorSettings {
  image: ProviderSettings;
  video: ProviderSettings;
  ffmpeg: boolean;
  ffprobe: boolean;
}

/** Patch shape for PUT settings — leave `api_key` blank/omitted to keep it. */
export interface ProviderSettingsPatch {
  enabled?: boolean;
  provider?: string;
  model?: string;
  base_url?: string;
  size?: string;
  seconds?: number;
  api_key?: string;
}

export interface CreatorSettingsPatch {
  image?: ProviderSettingsPatch;
  video?: ProviderSettingsPatch;
}

export type ActionName =
  | "generate_reference"
  | "generate_keyframe"
  | "generate_video"
  | "poll_video"
  | "assemble";

export interface ActionRequest {
  action: ActionName;
  target_id?: string;
}

export interface RunRequest {
  stage: Stage;
  publish_mode: PublishMode;
}

export interface SocialAccount {
  id: string;
  platform: string;
  display_name: string;
  username: string;
  profile_url: string;
  status: string;
}

/**
 * Cron.Meta as persisted server-side. The POST payload flattens these five
 * fields to top level (see `createCronJob`); this shape is only what GET
 * returns nested under `meta`.
 */
export interface CronMeta {
  role?: string;
  workspace?: string;
  content_project_id?: string;
  content_stage?: Stage;
  publish_mode?: PublishMode;
}

export interface CronJob {
  id: string;
  name: string;
  schedule: string;
  prompt: string;
  enabled: boolean;
  target: string;
  last_run: string | null;
  next_run: string | null;
  last_state: string;
  meta?: CronMeta;
}

/**
 * Top-level fields on POST /api/cron/jobs. CreatorCron accepts these
 * flattened; the server persists them into `job.meta` for later reads.
 */
export interface CronJobDraft {
  name: string;
  schedule: string;
  prompt?: string;
  timezone?: string;
  role: "content-creator";
  content_project_id: string;
  content_stage: Stage;
  publish_mode: PublishMode;
  workspace?: string;
}

const BASE = "/content-creator";

export const listProjects = async (): Promise<Project[]> => {
  const r = await get<{ projects: Project[] }>(`${BASE}/projects`);
  return r.projects ?? [];
};

export const getProject = (id: string) =>
  get<Project>(`${BASE}/projects/${encodeURIComponent(id)}`);

export const createProject = (p: Partial<Project>) =>
  post<Project>(`${BASE}/projects`, p);

export const updateProject = (id: string, p: Project) =>
  put<Project>(`${BASE}/projects/${encodeURIComponent(id)}`, p);

export const runAction = (id: string, body: ActionRequest) =>
  post<Project>(`${BASE}/projects/${encodeURIComponent(id)}/action`, body);

export const runStage = (id: string, body: RunRequest) =>
  post<{ session_id: string }>(
    `${BASE}/projects/${encodeURIComponent(id)}/run`,
    body,
  );

export const getSettings = () => get<CreatorSettings>(`${BASE}/settings`);

export const saveSettings = (patch: CreatorSettingsPatch) =>
  post<CreatorSettings>(`${BASE}/settings`, patch);

/** Existing picker endpoint — Main owns the /social/accounts route. */
export const listSocialAccounts = async (): Promise<SocialAccount[]> => {
  return get<SocialAccount[]>("/social/accounts");
};

export const listCronJobs = async (): Promise<CronJob[]> => {
  const r = await get<{ jobs: CronJob[] }>("/cron/jobs");
  return r.jobs ?? [];
};

export const createCronJob = (draft: CronJobDraft) =>
  post<CronJob>("/cron/jobs", draft);

/**
 * Auth-attaching URL for a registered project artifact (image/video/final).
 * Uses the shared `authedUrl` helper so the dashboard token rides along on
 * `<img>` / `<video>` / download `<a>` — request paths that cannot set
 * `Authorization`. Only registered artifact paths resolve server-side; the
 * service rejects arbitrary or traversal targets.
 */
export const artifactUrl = (projectId: string, path: string): string =>
  authedUrl(
    `${BASE}/projects/${encodeURIComponent(projectId)}/artifact?path=${encodeURIComponent(path)}`,
  );
