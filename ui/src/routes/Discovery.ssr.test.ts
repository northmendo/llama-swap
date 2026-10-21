import { describe, expect, it } from "vitest";
import { render } from "svelte/server";

/**
 * SSR smoke test for the Discovery view: the route compiles and
 * renders its chrome (title, scan button, empty-state explainer)
 * before any data load runs. The interactive flows are covered by the
 * agentApi client, engineUpdates and modelFiles tests.
 */
import Discovery from "./Discovery.svelte";

describe("Discovery view", () => {
  it("renders the header, the scan button and the empty state", () => {
    const { body } = render(Discovery);
    expect(body).toContain("Discovery");
    expect(body).toContain("Scan models");
    expect(body).toContain("Scan the folders");
  });
});
