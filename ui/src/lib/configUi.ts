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

/** The four selector strategies, as a compile-time type only. The list the
 * pickers render comes from the server (GET /agent/v1/config/schema), which
 * serves the validator's own config.SelectorStrategies. */
export type SelectorStrategy = "warm" | "pin" | "spillover" | "failover";

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

// ---------------------------------------------------------------------------
// Live provider pick list (peer models, selector targets)
// ---------------------------------------------------------------------------

/** One pickable model ID, with an optional hint line (owner, context). */
export interface ModelPickOption {
  id: string;
  hint?: string;
}

/** Case-insensitive substring filter over a pick list — the searchable
 * dropdown's whole matching rule, kept pure so it is testable. */
export function filterPickOptions(options: ModelPickOption[], query: string): ModelPickOption[] {
  const needle = query.trim().toLowerCase();
  if (!needle) return options;
  return options.filter((opt) => opt.id.toLowerCase().includes(needle));
}

/** Append one model ID to a comma-separated form list, skipping a
 * duplicate. Returns the new text so Svelte's $state picks it up. */
export function appendModelToText(text: string, id: string): string {
  if (modelsFromText(text).includes(id)) return text;
  const trimmed = text.trim();
  if (!trimmed) return id;
  if (trimmed.endsWith(",")) return trimmed + " " + id;
  return trimmed + ", " + id;
}

/** Hint line for one catalog entry: owner and/or context length. */
export function pickHint(ownedBy?: string, contextLength?: number): string | undefined {
  const parts: string[] = [];
  if (ownedBy) parts.push(ownedBy);
  if (contextLength && contextLength > 0) {
    parts.push(`ctx ${contextLength.toLocaleString("en-US")}`);
  }
  return parts.length > 0 ? parts.join(" · ") : undefined;
}

/** Pick options for selector targets: local models as-is, and a peer's
 * live catalog entries as peerName/modelId fully qualified names (the
 * routing form config.ResolvePeerModel accepts). */
export function targetPickOptions(
  localModels: string[],
  peerCatalogs: { peer: string; models: string[] }[],
): ModelPickOption[] {
  const out: ModelPickOption[] = localModels.map((id) => ({ id }));
  const seen = new Set(localModels);
  for (const { peer, models } of peerCatalogs) {
    for (const model of models) {
      const fqn = `${peer}/${model}`;
      if (seen.has(fqn)) continue;
      seen.add(fqn);
      out.push({ id: fqn, hint: `peers.${peer}` });
    }
  }
  return out;
}

// ---------------------------------------------------------------------------
// Grouped pick lists (peers.models, selector targets) — fed by the server's
// cached provider catalogs, so a pick list never probes a provider on load.
// ---------------------------------------------------------------------------

/** One heading group of a pick list: a source (provider, or "this server"). */
export interface PickGroup {
  label: string;
  options: ModelPickOption[];
}

/** Case-insensitive substring filter across groups, dropping empty groups. */
export function filterPickGroups(groups: PickGroup[], query: string): PickGroup[] {
  const needle = query.trim().toLowerCase();
  return groups
    .map((group) => ({
      label: group.label,
      options: needle
        ? group.options.filter((opt) => opt.id.toLowerCase().includes(needle))
        : group.options,
    }))
    .filter((group) => group.options.length > 0);
}

/** The model IDs a peer's cached catalog holds ([] when never checked). */
export function catalogIds(models: { id: string }[] | undefined): string[] {
  return (models ?? []).map((model) => model.id);
}

/** One group per provider, from the cached catalogs. Providers that were
 * never checked (or failed) simply contribute nothing. */
export function catalogGroups(
  catalogs: { name: string; models: { id: string; ownedBy?: string; contextLength?: number }[] }[],
): PickGroup[] {
  return catalogs
    .filter((entry) => entry.models.length > 0)
    .map((entry) => ({
      label: `peers.${entry.name}`,
      options: entry.models.map((model) => ({
        id: model.id,
        hint: pickHint(model.ownedBy, model.contextLength),
      })),
    }));
}

/** Selector targets as groups: "this server" first, then one per provider
 * (FQNs in the peerName/modelId wire form). Mirrors targetPickOptions, but
 * keeps the source boundaries so a picker can filter by target type. */
export function targetGroups(localModels: string[], catalogs: PickGroup[]): PickGroup[] {
  const groups: PickGroup[] = [];
  if (localModels.length > 0) {
    groups.push({ label: "This server", options: localModels.map((id) => ({ id })) });
  }
  for (const group of catalogs) {
    const peerName = group.label.replace(/^peers\./, "");
    groups.push({
      label: group.label,
      options: group.options.map((opt) => ({ id: `${peerName}/${opt.id}`, hint: group.label })),
    });
  }
  return groups;
}

/** Which source one existing target came from. Unknown targets (a peer that
 * is no longer configured) keep the provider form when they look qualified. */
export function targetSource(
  target: string,
  localModels: string[],
  peerNames: string[],
): "local" | "provider" {
  if (localModels.includes(target)) return "local";
  for (const peer of peerNames) {
    if (target.startsWith(peer + "/")) return "provider";
  }
  return target.includes("/") ? "provider" : "local";
}

/** The target picker's groups filtered to one target type: "local" keeps
 * this server's own models, "provider" keeps the peer catalogs. */
export function groupsForSource(groups: PickGroup[], source: "local" | "provider"): PickGroup[] {
  return groups.filter((group) =>
    source === "local" ? group.label === "This server" : group.label.startsWith("peers."),
  );
}

// ---------------------------------------------------------------------------
// The finite value sets the config screen's pickers are fed from
// ---------------------------------------------------------------------------

/**
 * Fallback selector strategies. The server's GET /agent/v1/config/schema is
 * authoritative (it serves config.SelectorStrategies, the list the
 * validator itself uses); this copy is only what a picker shows when that
 * request failed, so the form never renders empty.
 */
export const FALLBACK_SELECTOR_STRATEGIES = ["warm", "pin", "spillover", "failover"] as const;

export const DEFAULT_SELECTOR_STRATEGY = "failover";

/** The strategy options to render: the server's list when it arrived. */
export function strategyOptions(server: string[] | undefined): string[] {
  const list = (server ?? []).filter((s) => s.trim().length > 0);
  return list.length > 0 ? list : [...FALLBACK_SELECTOR_STRATEGIES];
}

/** The sentinel value of the proxy select's "custom…" row. */
export const PROXY_CUSTOM = "__custom__";

export interface ProxyChoice {
  value: string;
  label: string;
  /** True for a proxy another peer in the config already uses. */
  configured: boolean;
}

/**
 * Proxy picker options: the proxies already configured (each peer's, with
 * the peer names that use it), then the server's presets, then "custom…".
 */
export function proxyChoices(
  peers: { name: string; proxy: string }[],
  presets: { value: string; label: string }[],
): ProxyChoice[] {
  const out: ProxyChoice[] = [];
  const seen = new Set<string>();
  for (const peer of peers) {
    const proxy = peer.proxy.trim();
    if (!proxy) continue;
    if (seen.has(proxy)) {
      const existing = out.find((choice) => choice.value === proxy);
      if (existing) existing.label = `${existing.label.split(" (")[0]}, ${peer.name}`;
      continue;
    }
    seen.add(proxy);
    out.push({ value: proxy, label: `${proxy} (${peer.name})`, configured: true });
  }
  for (const preset of presets) {
    if (seen.has(preset.value)) continue;
    seen.add(preset.value);
    out.push({ value: preset.value, label: `${preset.label} — ${preset.value}`, configured: false });
  }
  return out;
}

// ---------------------------------------------------------------------------
// The peers form's key picker: macro NAMES only, never values
// ---------------------------------------------------------------------------

/** The key select's sentinels. */
export const KEY_NONE = "__none__";
export const KEY_CUSTOM = "__custom__";
export const KEY_LITERAL = "__literal__";

export interface KeyChoice {
  value: string;
  label: string;
}

/**
 * Key picker options: every ${env.VAR} name the server offers (flagged when
 * it does not resolve), the macro the form already holds even when the
 * server did not list it, then the clear / custom / literal fallbacks.
 */
export function keyChoices(
  envNames: { name: string; set: boolean }[],
  current: string,
): KeyChoice[] {
  const out: KeyChoice[] = [{ value: KEY_NONE, label: "No key" }];
  const seen = new Set<string>();
  for (const env of envNames) {
    const value = `\${env.${env.name}}`;
    if (seen.has(value)) continue;
    seen.add(value);
    out.push({ value, label: env.set ? env.name : `${env.name} (unavailable)` });
  }
  if (current && !seen.has(current)) {
    out.push({ value: current, label: `${current} (not in the server environment list)` });
  }
  out.push({ value: KEY_CUSTOM, label: "Other env macro…" });
  out.push({ value: KEY_LITERAL, label: "Literal (discouraged — rejected)" });
  return out;
}

/** The key select value matching an arbitrary key field value. */
export function keyChoiceFor(value: string, options: KeyChoice[]): string {
  if (!value) return KEY_NONE;
  return options.some((choice) => choice.value === value) ? value : KEY_CUSTOM;
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
