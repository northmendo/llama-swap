/**
 * Typed client for llama-swap's /agent/v1 REST API: the engines section,
 * the engine updater and the discovery/draft machinery.
 *
 * Every request carries the bearer token from stores/agentApiToken
 * (user-entered, kept in localStorage, never bundled). A 401 rejects
 * with AgentUnauthorizedError so views can surface the token prompt
 * again; other non-2xx responses reject with AgentApiError carrying
 * the server's error envelope message.
 *
 * Wire shapes mirror internal/agentapi (see
 * docs/kb/guides/api-integration/agent-api.md).
 */

import { apiUrl } from "./apiPath";
import { agentToken } from "../stores/agentApiToken";
import { get } from "svelte/store";

/** Any non-2xx /agent/v1 response; message comes from the error envelope. */
export class AgentApiError extends Error {
  readonly status: number;

  constructor(status: number, message: string) {
    super(message);
    this.name = "AgentApiError";
    this.status = status;
  }
}

/**
 * A 401 — the token is missing, wrong, or the server's key changed.
 * Also thrown locally when no token is stored, before any request is
 * made. Views catch this to (re-)open the token dialog.
 */
export class AgentUnauthorizedError extends AgentApiError {
  constructor(message = "unauthorized: missing or invalid bearer token") {
    super(401, message);
    this.name = "AgentUnauthorizedError";
  }
}

export interface EngineInfo {
  name: string;
  label: string;
  cmd: string;
  envKeys: string[];
}

export interface EngineUpdateInfo {
  name: string;
  label: string;
  hasUpdateSource: boolean;
  installedVersion?: string;
  source?: string;
  lastCheckAt?: string;
  lastUpdateAt?: string;
  updateAvailable?: { tag: string; url?: string; checkedAt: string };
}

export interface EngineCheckResult {
  engine: string;
  tag?: string;
  updateAvailable: boolean;
  upToDate: boolean;
  note?: string;
  error?: string;
}

export interface EngineUpdateResult {
  engine: string;
  approved: boolean;
  tag?: string;
  note?: string;
  updateAvailable: boolean;
  upToDate: boolean;
  installedVersion?: string;
  asset?: string;
  stoppedModels?: string[];
  prevSlot?: string;
  error?: string;
}

export interface EngineRollbackResult {
  engine: string;
  approved: boolean;
  restoredFrom?: string;
  stoppedModels?: string[];
}

export interface DiscoveryMetadata {
  path: string;
  fileSize: number;
  ggufVersion: number;
  architecture: string;
  type: string;
  contextLength: number;
  blockCount: number;
  headCount: number;
  headCountKv: number;
  expertCount: number;
  expertUsedCount: number;
  moe: boolean;
  mtpKeys?: Record<string, unknown>;
  quantHint: string;
  extraKeys: string[];
}

export interface ModelDraft {
  modelId: string;
  engine: string;
  engineSource?: string;
  args: string;
  ctxSize: number;
  ctxCapped: boolean;
  yaml: string;
}

export interface ScanFileResult {
  path: string;
  status: string;
  metadata?: DiscoveryMetadata;
  metadataError?: string;
  draft?: ModelDraft;
  engineSource?: string;
  skipReason?: string;
}

export interface ScanResponse {
  found: number;
  drafted: number;
  skipped: number;
  results: ScanFileResult[];
}

export interface ValidationResponse {
  valid: boolean;
  errors: string[];
}

export interface ApplyResponse {
  status: string;
  model: string;
  approved: boolean;
  written: boolean;
  reloadRequired: boolean;
  backup?: string;
  message?: string;
}

export interface RefineResponse extends ModelDraft {
  refined: boolean;
  reason?: string;
}

/** Reads the stored token without a store subscription. */
export function currentToken(): string | null {
  return get(agentToken);
}

async function parseEnvelope(resp: Response): Promise<string | null> {
  try {
    const body: unknown = await resp.json();
    const message = (body as { error?: { message?: string } })?.error?.message;
    return typeof message === "string" ? message : null;
  } catch {
    return null;
  }
}

/**
 * One authenticated JSON call against /agent/v1. Throws
 * AgentUnauthorizedError on 401 (and when no token is stored) and
 * AgentApiError on any other non-2xx, with the server's message.
 */
async function agentRequest<T>(method: string, path: string, body?: unknown): Promise<T> {
  const token = currentToken();
  if (!token) {
    throw new AgentUnauthorizedError("no agent API token set");
  }

  const headers: Record<string, string> = { Authorization: `Bearer ${token}` };
  let payload: string | undefined;
  if (body !== undefined) {
    headers["Content-Type"] = "application/json";
    payload = JSON.stringify(body);
  }

  const resp = await fetch(apiUrl(path), { method, headers, body: payload });

  if (resp.status === 401) {
    throw new AgentUnauthorizedError((await parseEnvelope(resp)) ?? undefined);
  }
  if (!resp.ok) {
    const message = (await parseEnvelope(resp)) ?? `agent API request failed (${resp.status})`;
    throw new AgentApiError(resp.status, message);
  }

  return (await resp.json()) as T;
}

/** GET /agent/v1/engines — name, label, cmd path and env var names. */
export async function listEngines(): Promise<EngineInfo[]> {
  const data = await agentRequest<{ engines: EngineInfo[] }>("GET", "/agent/v1/engines");
  return data.engines ?? [];
}

/** GET /agent/v1/engines/updates — installed versions and check results. */
export async function listEngineUpdates(): Promise<EngineUpdateInfo[]> {
  const data = await agentRequest<{ engines: EngineUpdateInfo[] }>("GET", "/agent/v1/engines/updates");
  return data.engines ?? [];
}

/**
 * POST /agent/v1/engines/check — refresh the update state from the
 * sources of the named engines (all engines with a source when the
 * list is empty). Never downloads anything.
 */
export async function checkEngineUpdates(engines: string[] = []): Promise<EngineCheckResult[]> {
  const data = await agentRequest<{ results: EngineCheckResult[] }>(
    "POST",
    "/agent/v1/engines/check",
    { engines },
  );
  return data.results ?? [];
}

/**
 * POST /agent/v1/engines/{name}/update — with approved=false (or a
 * check via checkEngineUpdates) it is check-only; with approved=true it
 * downloads, verifies and swaps the binary, stopping running models
 * first when force is set.
 */
export async function updateEngine(
  name: string,
  opts: { approved: boolean; force?: boolean } = { approved: false },
): Promise<EngineUpdateResult> {
  return agentRequest<EngineUpdateResult>(
    "POST",
    `/agent/v1/engines/${encodeURIComponent(name)}/update`,
    { approved: opts.approved, force: opts.force ?? false },
  );
}

/**
 * POST /agent/v1/engines/{name}/rollback — restore the previous binary.
 * Must be approved; force stops running models using the engine first.
 */
export async function rollbackEngine(
  name: string,
  opts: { approved: boolean; force?: boolean } = { approved: false },
): Promise<EngineRollbackResult> {
  return agentRequest<EngineRollbackResult>(
    "POST",
    `/agent/v1/engines/${encodeURIComponent(name)}/rollback`,
    { approved: opts.approved, force: opts.force ?? false },
  );
}

/** POST /agent/v1/scan — run discovery over the configured folders. */
export async function scanModels(): Promise<ScanResponse> {
  return agentRequest<ScanResponse>("POST", "/agent/v1/scan", {});
}

/** POST /agent/v1/validate — validate a stanza or full config, no side effects. */
export async function validateYaml(yaml: string): Promise<ValidationResponse> {
  return agentRequest<ValidationResponse>("POST", "/agent/v1/validate", { yaml });
}

/** POST /agent/v1/apply — merge an approved model stanza into the config file. */
export async function applyModel(
  yaml: string,
  approved: boolean,
): Promise<ApplyResponse> {
  return agentRequest<ApplyResponse>("POST", "/agent/v1/apply", { yaml, approved });
}

/**
 * POST /agent/v1/refine — draft + refine a stanza for one model file.
 * Rejects with a 400 AgentApiError when no draft assistant is
 * configured; callers hide the button in that case.
 */
export async function refineDraft(path: string): Promise<RefineResponse> {
  return agentRequest<RefineResponse>("POST", "/agent/v1/refine", { path });
}
