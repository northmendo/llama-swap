/**
 * Shared auth for llama-swap's own API surface (/v1/..., /api/...,
 * /logs/...) when the config sets `apiKeys`.
 *
 * The bearer token is the same one the /agent/v1 views use
 * (stores/agentApiToken: user-entered, kept in localStorage, never
 * bundled). With no token stored the request goes out keyless, exactly
 * like before, so a keyless server keeps working unchanged.
 *
 * A 401 sets apiAuthPrompt with the reason; App.svelte renders the
 * existing AgentTokenDialog for it, so the key can be entered at the
 * point of need instead of the call failing with a bare 401.
 */

import { get, writable } from "svelte/store";
import { agentToken } from "../stores/agentApiToken";

/** Why the token dialog is being shown; null when nothing prompted it. */
export const apiAuthPrompt = writable<string | null>(null);

/** Show the token dialog (the dialog clears this when it closes). */
export function promptForApiToken(reason: string): void {
  apiAuthPrompt.set(reason);
}

/** Dismiss the token prompt without touching the stored token. */
export function dismissApiTokenPrompt(): void {
  apiAuthPrompt.set(null);
}

/** The stored bearer token, or null when none is set. */
export function currentApiToken(): string | null {
  return get(agentToken);
}

/**
 * Merge the stored token's Authorization header into base (a plain
 * header object — every call site in this codebase uses one). With no
 * token stored, base is returned unchanged.
 */
export function apiAuthHeaders(base: Record<string, string> = {}): Record<string, string> {
  const token = currentApiToken();
  if (!token) return base;
  return { ...base, Authorization: `Bearer ${token}` };
}

/** The reason shown when llama-swap's own API rejects the UI's calls. */
const API_KEY_PROMPT_REASON =
  "llama-swap rejected this request: its API key (the apiKeys list in the config) is missing or wrong. Enter it once; it is stored in this browser only.";

/**
 * fetch() for llama-swap's own endpoints: attaches the stored bearer
 * token and surfaces a 401 through the token prompt. The Response is
 * returned untouched, so callers keep their existing error handling.
 */
export async function authedFetch(url: string, init: RequestInit = {}): Promise<Response> {
  const headers = apiAuthHeaders((init.headers as Record<string, string> | undefined) ?? {});
  const response = await fetch(url, { ...init, headers });
  if (response.status === 401) {
    promptForApiToken(API_KEY_PROMPT_REASON);
  }
  return response;
}
