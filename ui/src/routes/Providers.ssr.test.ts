import { describe, expect, it } from "vitest";
import { render } from "svelte/server";

/**
 * SSR smoke tests for the Providers view. The interactive flows (per-row
 * checks, inline 502s, copy-on-click) are covered by the agent API client and
 * the catalog cache tests in internal/agentapi; these assert the route
 * compiles and renders its chrome without touching the network.
 */
import Providers from "./Providers.svelte";

describe("Providers view", () => {
  it("renders its header and the check affordances", () => {
    const { body } = render(Providers);
    expect(body).toContain("Providers");
    expect(body).toContain("which models each configured provider actually serves");
    expect(body).toContain("Check all");
    expect(body).toContain("Refresh");
  });

  it("renders an empty state when the config has no peers", () => {
    const { body } = render(Providers);
    expect(body).toContain("No peers are defined in the config");
  });

  it("renders a row per peer: proxy host, counts, check button and key MACRO only", () => {
    const { body } = render(Providers, {
      props: {
        initialPeers: [
          {
            name: "healthy",
            proxy: "http://127.0.0.1:9393",
            keyMacro: "${env.PEER_KEY}",
            keyMacroAvailable: true,
            models: [
              { id: "zephyr-7b", ownedBy: "mock" },
              { id: "qwen3-coder-30b", ownedBy: "mock" },
              { id: "llama-4-scout", ownedBy: "mock" },
            ],
            modelCount: 3,
            latencyMs: 4,
            checkedAt: "2026-10-25T20:22:10Z",
          },
          {
            name: "deadhost",
            proxy: "http://127.0.0.1:9394",
            models: [],
            modelCount: 1,
          },
        ],
      },
    });
    expect(body).toContain("healthy");
    // The proxy host, not the full URL, and never a key value.
    expect(body).toContain("127.0.0.1:9393");
    expect(body).toContain("3 models");
    expect(body).toContain("Check models");
    expect(body).toContain("${env.PEER_KEY}");
    expect(body).toContain("macro available");
    expect(body).toContain("deadhost");
    expect(body).toContain("never checked");
    expect(body).toContain("no key configured");
  });

  it("flags a key macro that no longer resolves", () => {
    const { body } = render(Providers, {
      props: {
        initialPeers: [
          {
            name: "gone",
            proxy: "https://api.example.com",
            keyMacro: "${env.GONE_KEY}",
            keyMacroAvailable: false,
            models: [],
            modelCount: 1,
          },
        ],
      },
    });
    expect(body).toContain("${env.GONE_KEY}");
    expect(body).toContain("macro not resolvable");
  });
});

