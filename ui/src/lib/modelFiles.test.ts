import { describe, expect, it } from "vitest";
import { basename, formatBytes, formatContext } from "./modelFiles";

describe("basename", () => {
  it("keeps bare filenames", () => {
    expect(basename("saluki.gguf")).toBe("saluki.gguf");
  });

  it("cuts unix and windows separators", () => {
    expect(basename("/models/qwen/saluki-Q4.gguf")).toBe("saluki-Q4.gguf");
    expect(basename("C:\\models\\saluki.gguf")).toBe("saluki.gguf");
  });
});

describe("formatBytes", () => {
  it("renders a dash for nothing", () => {
    expect(formatBytes(0)).toBe("—");
    expect(formatBytes(-5)).toBe("—");
    expect(formatBytes(Number.NaN)).toBe("—");
  });

  it("scales through the binary units", () => {
    expect(formatBytes(512)).toBe("512 B");
    expect(formatBytes(2048)).toBe("2.0 KiB");
    expect(formatBytes(5 * 1024 * 1024)).toBe("5.0 MiB");
    expect(formatBytes(3.5 * 1024 ** 3)).toBe("3.5 GiB");
    expect(formatBytes(2 * 1024 ** 4)).toBe("2.0 TiB");
  });

  it("drops decimals for large values", () => {
    expect(formatBytes(123 * 1024)).toBe("123 KiB");
  });
});

describe("formatContext", () => {
  it("renders a dash for zero", () => {
    expect(formatContext(0, false)).toBe("—");
  });

  it("marks capped context sizes", () => {
    expect(formatContext(8192, false)).toBe("8,192");
    expect(formatContext(8192, true)).toBe("8,192 (capped)");
  });
});
