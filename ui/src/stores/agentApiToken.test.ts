import { describe, expect, it, vi } from "vitest";

/**
 * Token persistence semantics. The store module reads localStorage at
 * import time, so window/localStorage are stubbed before the dynamic
 * import below. The key values here are dummies; no real key material
 * exists anywhere in the repo.
 */

class MemoryStorage {
  private map = new Map<string, string>();

  getItem(key: string): string | null {
    return this.map.get(key) ?? null;
  }

  setItem(key: string, value: string): void {
    this.map.set(key, value);
  }

  removeItem(key: string): void {
    this.map.delete(key);
  }
}

const storage = new MemoryStorage();
vi.stubGlobal("window", { localStorage: storage });

const { agentToken, setAgentToken, clearAgentToken } = await import("./agentApiToken");

function stored(): string | null {
  return storage.getItem("llama-swap.agentApiToken");
}

function current(): string | null {
  let value: string | null = null;
  const unsubscribe = agentToken.subscribe((v) => (value = v));
  unsubscribe();
  return value;
}

describe("agentApiToken store", () => {
  it("stores a trimmed token in localStorage and publishes it", () => {
    const seen: (string | null)[] = [];
    const unsubscribe = agentToken.subscribe((v) => seen.push(v));

    setAgentToken("  key-one  ");
    expect(stored()).toBe("key-one");
    clearAgentToken();
    expect(stored()).toBeNull();

    unsubscribe();
    expect(seen).toEqual([null, "key-one", null]);
  });

  it("treats empty and whitespace-only tokens as cleared", () => {
    setAgentToken("real");
    setAgentToken("   ");
    expect(stored()).toBeNull();
    expect(current()).toBeNull();
  });

  it("removes the stored entry rather than blanking it", () => {
    setAgentToken("real");
    clearAgentToken();
    expect(stored()).toBeNull();
  });

  it("works without a window (memory only, nothing throws)", async () => {
    vi.unstubAllGlobals();
    vi.resetModules();
    const fresh = await import("./agentApiToken");
    fresh.setAgentToken("memory-only");
    let value: string | null = null;
    const unsubscribe = fresh.agentToken.subscribe((v) => (value = v));
    unsubscribe();
    expect(value).toBe("memory-only");
    vi.stubGlobal("window", { localStorage: storage });
  });
});
