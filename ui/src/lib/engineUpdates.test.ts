import { describe, expect, it } from "vitest";
import {
  formatTimestamp,
  refusalMessage,
  summarizeCheckResult,
  summarizeRollbackResult,
  summarizeUpdateResult,
} from "./engineUpdates";
import type { EngineCheckResult, EngineRollbackResult, EngineUpdateResult } from "./agentApi";

describe("summarizeCheckResult", () => {
  it("reports a failed source as a negative outcome", () => {
    const row: EngineCheckResult = { engine: "beellama", updateAvailable: false, upToDate: false, error: "boom" };
    const out = summarizeCheckResult(row);
    expect(out.ok).toBe(false);
    expect(out.text).toContain("boom");
  });

  it("reports up-to-date", () => {
    const row: EngineCheckResult = { engine: "beellama", updateAvailable: false, upToDate: true };
    expect(summarizeCheckResult(row).text).toBe("checked: up to date");
  });

  it("reports an available update with its tag", () => {
    const row: EngineCheckResult = { engine: "beellama", tag: "b13", updateAvailable: true, upToDate: false };
    const out = summarizeCheckResult(row);
    expect(out.ok).toBe(true);
    expect(out.text).toBe("update available: b13");
  });

  it("falls back to the note, then to a default", () => {
    const withNote: EngineCheckResult = { engine: "x", updateAvailable: false, upToDate: false, note: "direct url changed" };
    expect(summarizeCheckResult(withNote).text).toBe("checked: direct url changed");
    const bare: EngineCheckResult = { engine: "x", updateAvailable: false, upToDate: false };
    expect(summarizeCheckResult(bare).text).toBe("checked: no update found");
  });
});

describe("summarizeUpdateResult", () => {
  it("details what an approved update did", () => {
    const res: EngineUpdateResult = {
      engine: "beellama",
      approved: true,
      installedVersion: "b13",
      asset: "llama-server-linux-amd64",
      prevSlot: "/opt/llama-server.prev",
      stoppedModels: ["mudi", "saluki"],
      updateAvailable: false,
      upToDate: false,
    };
    const out = summarizeUpdateResult(res);
    expect(out.ok).toBe(true);
    expect(out.text).toBe("updated to b13");
    expect(out.detail).toContain("installed version: b13");
    expect(out.detail).toContain("asset: llama-server-linux-amd64");
    expect(out.detail).toContain("previous build kept at /opt/llama-server.prev");
    expect(out.detail).toContain("stopped models: mudi, saluki");
  });

  it("is negative when the result carries an error", () => {
    const res: EngineUpdateResult = {
      engine: "beellama",
      approved: true,
      error: "checksum mismatch",
      updateAvailable: false,
      upToDate: false,
    };
    const out = summarizeUpdateResult(res);
    expect(out.ok).toBe(false);
    expect(out.text).toContain("checksum mismatch");
    expect(out.detail).toContain("checksum mismatch");
  });

  it("says up to date when nothing was newer", () => {
    const res: EngineUpdateResult = { engine: "beellama", approved: true, upToDate: true, updateAvailable: false };
    expect(summarizeUpdateResult(res).detail).toContain("no newer release found");
  });
});

describe("summarizeRollbackResult", () => {
  it("details the restore and the reversibility", () => {
    const res: EngineRollbackResult = {
      engine: "beellama",
      approved: true,
      restoredFrom: "/opt/llama-server.prev",
      stoppedModels: ["saluki"],
    };
    const out = summarizeRollbackResult(res);
    expect(out.ok).toBe(true);
    expect(out.text).toBe("rolled back beellama");
    expect(out.detail).toContain("restored from /opt/llama-server.prev");
    expect(out.detail).toContain("stopped models: saluki");
    expect(out.detail.some((d) => d.includes("roll back again"))).toBe(true);
  });
});

describe("refusalMessage", () => {
  it("appends the force hint to a 409 refusal", () => {
    expect(refusalMessage("models saluki are running")).toBe(
      'models saluki are running — retry with "stop models" checked.',
    );
  });
});

describe("formatTimestamp", () => {
  it("renders empty as a dash and passes garbage through", () => {
    expect(formatTimestamp(undefined)).toBe("—");
    expect(formatTimestamp("")).toBe("—");
    expect(formatTimestamp("not-a-date")).toBe("not-a-date");
  });

  it("parses RFC3339 timestamps", () => {
    const out = formatTimestamp("2026-10-21T07:30:00Z");
    expect(out).not.toBe("—");
    expect(out).not.toBe("2026-10-21T07:30:00Z");
  });
});
