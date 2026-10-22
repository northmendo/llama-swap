import { describe, expect, it } from "vitest";
import { render } from "svelte/server";

/**
 * SSR smoke tests for the Config views: the route shell and the three
 * panels compile and render their chrome before any data load runs.
 * The interactive flows are covered by the configApi client tests and
 * the configUi helper tests.
 */
import Config from "./Config.svelte";
import PeersPanel from "../components/config/PeersPanel.svelte";
import CascadesPanel from "../components/config/CascadesPanel.svelte";
import AgentAPIPanel from "../components/config/AgentAPIPanel.svelte";

const noop = () => {};
const auth = () => false;

describe("Config view", () => {
  it("renders the header, subtitle and refresh button", () => {
    const { body } = render(Config);
    expect(body).toContain("Config");
    expect(body).toContain("peers, cascades and the agent API");
    expect(body).toContain("Refresh");
  });
});

describe("Peers panel", () => {
  it("renders an empty state when no peers are configured", () => {
    const { body } = render(PeersPanel, {
      props: { peers: [], onchanged: noop, onauth: auth, onerror: noop },
    });
    expect(body).toContain("No peers are defined");
    expect(body).toContain("Add peer");
  });

  it("renders peers with their key macros, never values", () => {
    const { body } = render(PeersPanel, {
      props: {
        peers: [
          {
            name: "upstream",
            proxy: "http://127.0.0.1:9393",
            apiKey: { macro: "${env.PEER_KEY}", set: true },
            tailcatKey: { set: false },
            models: ["remote-model"],
          },
        ],
        onchanged: noop,
        onauth: auth,
        onerror: noop,
      },
    });
    expect(body).toContain("upstream");
    expect(body).toContain("${env.PEER_KEY}");
    expect(body).toContain("remote-model");
  });
});

describe("Cascades panel", () => {
  it("renders selectors with strategy, ordered targets and health", () => {
    const { body } = render(CascadesPanel, {
      props: {
        selectors: [
          {
            name: "cascade",
            strategy: "failover",
            targets: ["existing", "upstream/remote-model"],
            spillover: 0,
            cooldown: "90s",
          },
        ],
        health: new Map([
          ["cascade", [
            { target: "existing", healthy: true },
            { target: "upstream/remote-model", healthy: false, lastError: "502 bad gateway" },
          ]],
        ]),
        onchanged: noop,
        onauth: auth,
        onerror: noop,
      },
    });
    expect(body).toContain("cascade");
    expect(body).toContain("failover");
    expect(body).toContain("existing");
    expect(body).toContain("upstream/remote-model");
    expect(body).toContain("502 bad gateway");
  });

  it("renders an empty state when no selectors are configured", () => {
    const { body } = render(CascadesPanel, {
      props: { selectors: [], onchanged: noop, onauth: auth, onerror: noop },
    });
    expect(body).toContain("No selectors are defined");
  });
});

describe("Agent API panel", () => {
  it("renders the section, key state, backups and restore buttons", () => {
    const { body } = render(AgentAPIPanel, {
      props: {
        agentApi: { enabled: true, key: { macro: "${env.AGENT_KEY}", set: true } },
        backups: [
          {
            name: "config.yaml.bak-20261022T071053Z",
            path: "/x/config.yaml.bak-20261022T071053Z",
            sizeBytes: 740,
            modifiedAt: "2026-10-22T07:10:53Z",
          },
        ],
        onchanged: noop,
        onauth: auth,
        onerror: noop,
      },
    });
    expect(body).toContain("Agent API");
    expect(body).toContain("${env.AGENT_KEY}");
    expect(body).toContain("Config backups");
    expect(body).toContain("config.yaml.bak-20261022T071053Z");
    expect(body).toContain("Restore");
  });

  it("renders a hint when no agent_api section exists", () => {
    const { body } = render(AgentAPIPanel, {
      props: { agentApi: null, backups: [], onchanged: noop, onauth: auth, onerror: noop },
    });
    expect(body).toContain("No agent_api");
  });
});
