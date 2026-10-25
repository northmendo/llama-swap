import { describe, expect, it } from "vitest";
import { render } from "svelte/server";

/**
 * The config screen's dropdowns must hydrate from the server's cached
 * provider catalogs (GET /agent/v1/config/peers.catalog) — no probing on page
 * load. These tests feed the exact shape that endpoint returns through the
 * same helpers the panels use, and assert the resulting option list.
 */
import ModelPicker from "./ModelPicker.svelte";
import { catalogGroups, targetGroups } from "../../lib/configUi";
import type { PeerCatalogEntry } from "../../lib/agentApi";

const catalogs: PeerCatalogEntry[] = [
  {
    name: "upstream",
    proxy: "http://127.0.0.1:9393",
    models: [
      { id: "qwen3-coder-30b", ownedBy: "acme" },
      { id: "llama-4-scout", ownedBy: "acme" },
    ],
    modelCount: 2,
    checkedAt: "2026-10-25T10:00:00Z",
  },
  {
    name: "deadhost",
    proxy: "http://127.0.0.1:9394",
    models: [],
    modelCount: 0,
  },
];

describe("Model picker", () => {
  it("lists the cached catalog's models, grouped by provider", () => {
    const { body } = render(ModelPicker, {
      props: { groups: catalogGroups(catalogs), onpick: () => {} },
    });
    expect(body).toContain("peers.upstream");
    // A peer with no cached catalog contributes no empty group.
    expect(body).not.toContain("peers.deadhost");
    expect(body).toContain("qwen3-coder-30b");
    expect(body).toContain("llama-4-scout");
    expect(body).toContain("acme");
  });

  it("offers peer-qualified targets for a cascade target picker", () => {
    const { body } = render(ModelPicker, {
      props: {
        groups: targetGroups(["local-model"], catalogGroups(catalogs)),
        onpick: () => {},
      },
    });
    expect(body).toContain("This server");
    expect(body).toContain("local-model");
    // The wire format the backend resolves: peerName/modelId.
    expect(body).toContain("upstream/qwen3-coder-30b");
  });

  it("renders the empty state instead of a dead list when nothing is cached", () => {
    const { body } = render(ModelPicker, {
      props: { groups: [], emptyText: "No cached provider catalog yet.", onpick: () => {} },
    });
    expect(body).toContain("No cached provider catalog yet.");
  });
});
