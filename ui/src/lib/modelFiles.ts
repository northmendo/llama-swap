/**
 * Pure path/size helpers for the Discovery view (GGUF scan results).
 * Kept free of Svelte so vitest can cover them directly.
 */

/** The final path segment of a model file path. */
export function basename(path: string): string {
  const cut = Math.max(path.lastIndexOf("/"), path.lastIndexOf("\\"));
  return cut < 0 ? path : path.slice(cut + 1);
}

/** Human-readable byte size; non-positive/non-finite → "—". */
export function formatBytes(size: number): string {
  if (!Number.isFinite(size) || size <= 0) return "—";
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let value = size;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit++;
  }
  return `${value >= 100 ? value.toFixed(0) : value.toFixed(1)} ${units[unit]}`;
}

/** Context size, marked when the discovery rules capped it. */
export function formatContext(ctx: number, capped: boolean): string {
  if (!ctx) return "—";
  return capped ? `${ctx.toLocaleString()} (capped)` : ctx.toLocaleString();
}
