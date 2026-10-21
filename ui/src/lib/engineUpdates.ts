/**
 * Pure presentation helpers for the Engines view: turning /agent/v1
 * engine updater responses into the status lines the view renders.
 * Kept free of Svelte so vitest can cover them directly.
 */

import type {
  EngineCheckResult,
  EngineRollbackResult,
  EngineUpdateResult,
} from "./agentApi";

export interface EngineOutcome {
  ok: boolean;
  /** One-line headline for the result box. */
  text: string;
  /** Detail lines rendered under the headline. */
  detail: string[];
}

/** A "Check for updates" row → headline (the check-all button uses ""). */
export function summarizeCheckResult(row: EngineCheckResult): EngineOutcome {
  if (row.error) {
    return { ok: false, text: `check failed: ${row.error}`, detail: [] };
  }
  if (row.upToDate) {
    return { ok: true, text: "checked: up to date", detail: [] };
  }
  if (row.updateAvailable) {
    return { ok: true, text: `update available: ${row.tag || "newer build"}`, detail: [] };
  }
  return { ok: true, text: `checked: ${row.note || "no update found"}`, detail: [] };
}

/** An approved (or check-only) update route response → result box. */
export function summarizeUpdateResult(res: EngineUpdateResult): EngineOutcome {
  const detail: string[] = [];
  if (res.installedVersion) detail.push(`installed version: ${res.installedVersion}`);
  if (res.asset) detail.push(`asset: ${res.asset}`);
  if (res.prevSlot) detail.push(`previous build kept at ${res.prevSlot}`);
  if (res.stoppedModels?.length) detail.push(`stopped models: ${res.stoppedModels.join(", ")}`);
  if (res.upToDate) detail.push("no newer release found");
  if (res.note) detail.push(res.note);
  if (res.error) detail.push(res.error);

  return res.error
    ? { ok: false, text: `update failed: ${res.error}`, detail }
    : { ok: true, text: `updated to ${res.installedVersion || "the latest matched release"}`, detail };
}

/** A rollback response → result box. */
export function summarizeRollbackResult(res: EngineRollbackResult): EngineOutcome {
  const detail: string[] = [];
  if (res.restoredFrom) detail.push(`restored from ${res.restoredFrom}`);
  if (res.stoppedModels?.length) detail.push(`stopped models: ${res.stoppedModels.join(", ")}`);
  detail.push("the newer build was kept as the new rollback slot; roll back again to redo it");
  return { ok: true, text: `rolled back ${res.engine}`, detail };
}

/**
 * The text shown when an update/rollback is refused with 409 because
 * models using the engine are running.
 */
export function refusalMessage(errMsg: string): string {
  return `${errMsg} — retry with "stop models" checked.`;
}

/** RFC3339 → locale string, "" and garbage → "—". SSR-safe. */
export function formatTimestamp(iso: string | undefined): string {
  if (!iso) return "—";
  const date = new Date(iso);
  return Number.isNaN(date.getTime()) ? iso : date.toLocaleString();
}
