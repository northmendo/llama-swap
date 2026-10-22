/**
 * Pure presentation/validation helpers for the Config views (peers,
 * cascades, agent API, backups). No Svelte and no fetch here, so vitest
 * covers the rules directly — most importantly the hard rule that key
 * fields only ever accept ${env.VAR} macro references, never literal
 * secrets.
 */

import type { ConfigBackup, ConfigWriteResponse, SecretRef } from "./agentApi";

/** Matches exactly one whole ${env.VAR_NAME} reference. */
export const ENV_MACRO_RE = /^\$\{env\.([A-Za-z_][A-Za-z0-9_]*)\}$/;

/**
 * The key-field rule: empty (clear) or a whole ${env.VAR} reference is
 * valid; anything else — a literal secret — is rejected before it is
 * ever sent to the server. Returns null when valid, the error message
 * otherwise.
 */
export function envMacroError(field: string, value: string): string | null {
  if (value === "" || ENV_MACRO_RE.test(value)) return null;
  return `${field} must reference an environment variable macro (e.g. \${env.MY_KEY}); literal secrets are never accepted by this UI`;
}

/** Human label for a key field: the macro when there is one, a bare
 * "set"/"not set" otherwise. The value itself is never known here. */
export function secretLabel(ref: SecretRef | undefined): string {
  if (!ref) return "not set";
  if (ref.macro) return ref.macro;
  return ref.set ? "set (value not shown)" : "not set";
}

/** The four selector strategies the config accepts. */
export const SELECTOR_STRATEGIES = ["warm", "pin", "spillover", "failover"] as const;

export type SelectorStrategy = (typeof SELECTOR_STRATEGIES)[number];

/** One target's failover health, from /v1/models meta.llamaswap.failover. */
export interface TargetHealth {
  target: string;
  healthy: boolean;
  retryAt?: number;
  lastError?: string;
}

/** The /v1/models metadata shape for a selector record. */
export interface SelectorMeta {
  type?: string;
  strategy?: string;
  targets?: string[];
  spillover?: number;
  cooldownSeconds?: number;
  failover?: TargetHealth[];
}

/**
 * The failover health of every selector, keyed by selector model ID,
 * extracted from a /v1/models payload. Selectors without failover
 * health (other strategies) are absent.
 */
export function failoverHealthFromModels(
  records: { id: string; meta?: { llamaswap?: SelectorMeta } }[],
): Map<string, TargetHealth[]> {
  const out = new Map<string, TargetHealth[]>();
  for (const record of records) {
    const meta = record.meta?.llamaswap;
    if (meta?.type === "selector" && meta.failover) {
      out.set(record.id, meta.failover);
    }
  }
  return out;
}

/** Health lookup for one target of one selector. */
export function healthFor(
  health: Map<string, TargetHealth[]> | undefined,
  selector: string,
  target: string,
): TargetHealth | undefined {
  return health?.get(selector)?.find((entry) => entry.target === target);
}

/** When an unhealthy target's cooldown expires, for a badge title. */
export function retryLabel(entry: TargetHealth | undefined): string | undefined {
  if (!entry || entry.healthy || !entry.retryAt) return undefined;
  const ms = entry.retryAt * 1000 - Date.now();
  if (ms <= 0) return "cooldown expired; will be retried next";
  const seconds = Math.ceil(ms / 1000);
  return `unhealthy — retrying in ~${seconds}s`;
}

/** Comma-or-newline separated model list ↔ string[] for the forms. */
export function modelsToText(models: string[] | undefined): string {
  return (models ?? []).join(", ");
}

export function modelsFromText(text: string): string[] {
  return text
    .split(/[,\n]/)
    .map((part) => part.trim())
    .filter((part) => part.length > 0);
}

export interface OutcomeBox {
  ok: boolean;
  text: string;
  detail: string[];
}

/**
 * A write verb's response → the result box the views render. The dry
 * run (status "pending") validates without writing; the applied call
 * reports the backup and whether a reload is still needed.
 */
export function describeWriteResult(res: ConfigWriteResponse): OutcomeBox {
  if (res.status === "pending") {
    return {
      ok: true,
      text: "validated — the change loads cleanly. Nothing was written yet; press Apply to write it.",
      detail: [],
    };
  }
  const detail: string[] = [];
  if (res.backup) detail.push(`backup: ${res.backup}`);
  if (res.reloadRequired) {
    detail.push("reload required: send SIGHUP or restart llama-swap to load it (--watch-config does it automatically)");
  } else if (res.written) {
    detail.push("watching the config: the change reloads automatically");
  }
  if (res.message) detail.push(res.message);
  return { ok: res.written, text: res.written ? "applied" : "not written", detail };
}

/** One backup row → the columns the table shows. */
export function backupRow(backup: ConfigBackup): { name: string; size: string; modified: string } {
  return {
    name: backup.name,
    size: `${(backup.sizeBytes / 1024).toFixed(1)} KiB`,
    modified: backup.modifiedAt,
  };
}
