import { writable, type Writable } from "svelte/store";

/**
 * The agent API bearer token (/agent/v1). The token is a secret: it is
 * kept in localStorage on the user's own device and never enters the
 * JS bundle, the build output, or any file in the repo. Empty and
 * whitespace-only values are treated as "no token".
 */

const STORAGE_KEY = "llama-swap.agentApiToken";

function readStoredToken(): string | null {
  if (typeof window === "undefined") return null;
  try {
    const value = window.localStorage.getItem(STORAGE_KEY);
    return value && value.trim() ? value : null;
  } catch {
    return null;
  }
}

/** The current token, null when unset. Persisted to localStorage. */
export const agentToken: Writable<string | null> = writable<string | null>(readStoredToken());

agentToken.subscribe((value) => {
  if (typeof window === "undefined") return;
  try {
    if (value && value.trim()) {
      window.localStorage.setItem(STORAGE_KEY, value);
    } else {
      window.localStorage.removeItem(STORAGE_KEY);
    }
  } catch (e) {
    console.error("agent API token: could not persist to localStorage", e);
  }
});

/** Save a token entered by the user (trimmed; empty clears it). */
export function setAgentToken(token: string): void {
  const trimmed = token.trim();
  agentToken.set(trimmed ? trimmed : null);
}

/** Forget the token (Settings-style "clear" affordance and 401 flows). */
export function clearAgentToken(): void {
  agentToken.set(null);
}
