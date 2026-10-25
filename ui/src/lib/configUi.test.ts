import { describe, expect, it } from "vitest";

/**
 * Tests for the Config view's pure helpers: the ${env.VAR}-only key
 * rule (the hard rule from F5 — the UI must never accept or show a
 * literal secret), the failover-health extraction from /v1/models, the
 * write-result outcome boxes and the form serialization helpers.
 */
import {
  ENV_MACRO_RE,
  appendModelToText,
  backupRow,
  describeWriteResult,
  envMacroError,
  failoverHealthFromModels,
  filterPickOptions,
  healthFor,
  modelsFromText,
  modelsToText,
  pickHint,
  retryLabel,
  secretLabel,
  targetPickOptions,
} from "./configUi";
import type { ConfigWriteResponse, SecretRef } from "./agentApi";

describe("envMacroError — the key-field hard rule", () => {
  it("accepts empty (clear) and whole env macros", () => {
    expect(envMacroError("apiKey", "")).toBeNull();
    expect(envMacroError("apiKey", "${env.MY_PEER_KEY}")).toBeNull();
    expect(envMacroError("apiKey", "${env.A_1}")).toBeNull();
  });

  it("rejects literal secrets before anything is sent", () => {
    expect(envMacroError("apiKey", "sk-literal-123")).toContain("macro");
    expect(envMacroError("key", "hunter2")).toContain("literal secrets");
  });

  it("rejects malformed macro shapes", () => {
    expect(envMacroError("apiKey", "${env}")).not.toBeNull();
    expect(envMacroError("apiKey", "prefix-${env.VAR}")).not.toBeNull();
    expect(envMacroError("apiKey", "${env.VAR} ")).not.toBeNull();
    expect(envMacroError("apiKey", "${env.1BAD}")).not.toBeNull();
    expect(ENV_MACRO_RE.test("${env.GOOD_NAME}")).toBe(true);
  });
});

describe("secretLabel", () => {
  it("shows the macro when the config references one", () => {
    const ref: SecretRef = { macro: "${env.PEER_KEY}", set: true };
    expect(secretLabel(ref)).toBe("${env.PEER_KEY}");
  });

  it("never shows a value — only set/not set", () => {
    expect(secretLabel({ set: true })).toBe("set (value not shown)");
    expect(secretLabel({ set: false })).toBe("not set");
    expect(secretLabel(undefined)).toBe("not set");
  });
});

describe("failoverHealthFromModels", () => {
  const records = [
    {
      id: "cascade",
      meta: {
        llamaswap: {
          type: "selector",
          strategy: "failover",
          targets: ["a", "peer/b"],
          failover: [
            { target: "a", healthy: true },
            { target: "peer/b", healthy: false, retryAt: 4102444800, lastError: "502" },
          ],
        },
      },
    },
    { id: "plain-model", meta: { llamaswap: { type: "model" } } },
    { id: "warm-selector", meta: { llamaswap: { type: "selector", strategy: "warm" } } },
    { id: "no-meta" },
  ];

  it("extracts failover health per selector, keyed by model ID", () => {
    const health = failoverHealthFromModels(records);
    expect(health.size).toBe(1);
    expect(health.get("cascade")).toEqual([
      { target: "a", healthy: true },
      { target: "peer/b", healthy: false, retryAt: 4102444800, lastError: "502" },
    ]);
  });

  it("looks up one target's health", () => {
    const health = failoverHealthFromModels(records);
    expect(healthFor(health, "cascade", "peer/b")?.healthy).toBe(false);
    expect(healthFor(health, "cascade", "missing")).toBeUndefined();
    expect(healthFor(undefined, "cascade", "a")).toBeUndefined();
  });

  it("labels an unhealthy target's cooldown, not a healthy one", () => {
    const health = failoverHealthFromModels(records);
    const unhealthy = healthFor(health, "cascade", "peer/b");
    const healthy = healthFor(health, "cascade", "a");
    expect(retryLabel(unhealthy)).toBeDefined();
    expect(retryLabel(unhealthy)).toContain("unhealthy");
    expect(retryLabel(healthy)).toBeUndefined();
    expect(retryLabel(undefined)).toBeUndefined();
  });
});

describe("modelsToText / modelsFromText", () => {
  it("round-trips comma and newline separated lists", () => {
    expect(modelsToText(["a", "b"])).toBe("a, b");
    expect(modelsFromText("a, b\nc ,  d")).toEqual(["a", "b", "c", "d"]);
    expect(modelsFromText("  ")).toEqual([]);
    expect(modelsFromText(modelsToText(["x"]))).toEqual(["x"]);
  });
});

describe("describeWriteResult", () => {
  it("renders the dry run as validated-but-not-written", () => {
    const res = { status: "pending", approved: false, written: false, reloadRequired: true } as ConfigWriteResponse;
    const box = describeWriteResult(res);
    expect(box.ok).toBe(true);
    expect(box.text).toContain("Nothing was written");
  });

  it("renders an applied write with backup and reload hint", () => {
    const res = {
      status: "applied",
      approved: true,
      written: true,
      reloadRequired: true,
      backup: "/etc/llama-swap/config.yaml.bak-20261022T071053Z",
      message: "model merged into the config file",
    } as ConfigWriteResponse;
    const box = describeWriteResult(res);
    expect(box.ok).toBe(true);
    expect(box.text).toBe("applied");
    expect(box.detail.some((line) => line.includes("config.yaml.bak-20261022T071053Z"))).toBe(true);
    expect(box.detail.some((line) => line.includes("reload required"))).toBe(true);
  });

  it("notes automatic reloads when the server watches the config", () => {
    const res = {
      status: "applied",
      approved: true,
      written: true,
      reloadRequired: false,
    } as ConfigWriteResponse;
    const box = describeWriteResult(res);
    expect(box.detail.some((line) => line.includes("automatically"))).toBe(true);
  });
});

describe("backupRow", () => {
  it("formats the size and passes the name and timestamp through", () => {
    const row = backupRow({
      name: "config.yaml.bak-20261022T071053Z",
      path: "/etc/llama-swap/config.yaml.bak-20261022T071053Z",
      sizeBytes: 740,
      modifiedAt: "2026-10-22T07:10:53Z",
    });
    expect(row.name).toBe("config.yaml.bak-20261022T071053Z");
    expect(row.size).toBe("0.7 KiB");
    expect(row.modified).toBe("2026-10-22T07:10:53Z");
  });
});

// ---------------------------------------------------------------------------
// Live provider pick list
// ---------------------------------------------------------------------------

describe("filterPickOptions", () => {
  const options = [
    { id: "llama-3.1-8b" },
    { id: "deepseek-chat", hint: "openrouter" },
    { id: "Qwen/Qwen3-32B" },
  ];

  it("returns everything for an empty query", () => {
    expect(filterPickOptions(options, "")).toEqual(options);
    expect(filterPickOptions(options, "   ")).toEqual(options);
  });

  it("matches case-insensitively as a substring", () => {
    expect(filterPickOptions(options, "DEEPSEEK").map((o) => o.id)).toEqual(["deepseek-chat"]);
    expect(filterPickOptions(options, "qwen3").map((o) => o.id)).toEqual(["Qwen/Qwen3-32B"]);
    expect(filterPickOptions(options, "llama").map((o) => o.id)).toEqual(["llama-3.1-8b"]);
  });

  it("matches nothing cleanly", () => {
    expect(filterPickOptions(options, "gpt-9")).toEqual([]);
  });
});

describe("appendModelToText", () => {
  it("appends to an empty list", () => {
    expect(appendModelToText("", "m1")).toBe("m1");
  });

  it("appends after existing entries with a comma", () => {
    expect(appendModelToText("m1", "m2")).toBe("m1, m2");
    expect(appendModelToText("m1, m2 ", "m3")).toBe("m1, m2, m3");
  });

  it("never duplicates an entry already in the list", () => {
    expect(appendModelToText("m1, m2", "m2")).toBe("m1, m2");
    expect(appendModelToText("m1,m2", "m1")).toBe("m1,m2");
  });

  it("does not double the comma when the text ends with one", () => {
    expect(appendModelToText("m1,", "m2")).toBe("m1, m2");
  });
});

describe("pickHint", () => {
  it("joins owner and context length", () => {
    expect(pickHint("openrouter", 131072)).toBe("openrouter · ctx 131,072");
  });

  it("handles either half missing", () => {
    expect(pickHint("vllm", undefined)).toBe("vllm");
    expect(pickHint(undefined, 4096)).toBe("ctx 4,096");
    expect(pickHint(undefined, 0)).toBeUndefined();
    expect(pickHint("", 0)).toBeUndefined();
  });
});

describe("targetPickOptions", () => {
  it("lists local models as-is and peer catalog entries as FQNs", () => {
    const opts = targetPickOptions(["local-a"], [{ peer: "upstream", models: ["remote-b"] }]);
    expect(opts).toEqual([
      { id: "local-a" },
      { id: "upstream/remote-b", hint: "peers.upstream" },
    ]);
  });

  it("dedupes a peer FQN that already exists as a local model", () => {
    const opts = targetPickOptions(["upstream/remote-b"], [
      { peer: "upstream", models: ["remote-b", "remote-c"] },
    ]);
    expect(opts.map((o) => o.id)).toEqual(["upstream/remote-b", "upstream/remote-c"]);
  });

  it("is fine with no peers configured", () => {
    expect(targetPickOptions(["local-a"], [])).toEqual([{ id: "local-a" }]);
  });
});
