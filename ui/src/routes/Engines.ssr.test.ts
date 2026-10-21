import { describe, expect, it } from "vitest";
import { render } from "svelte/server";

/**
 * SSR smoke test for the Engines view: the route compiles and renders
 * its chrome (title, check-all and refresh buttons) before any data
 * load runs. The interactive flows are covered by the agentApi client
 * and engineUpdates tests.
 */
import Engines from "./Engines.svelte";

describe("Engines view", () => {
  it("renders the header and the check-all / refresh buttons", () => {
    const { body } = render(Engines);
    expect(body).toContain("Engines");
    expect(body).toContain("Check all");
    expect(body).toContain("Refresh");
  });
});
