import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

/**
 * Tests for the /agent/v1 config-verb client: wire shapes, the dry-run
 * vs approved payloads, and the 401 path. fetch is mocked; the token is
 * driven through the real store (in-memory in node).
 */

let fetchMock: ReturnType<typeof vi.fn>;

beforeEach(async () => {
  fetchMock = vi.fn();
  vi.stubGlobal("fetch", fetchMock);

  const token = await import("../stores/agentApiToken");
  token.agentToken.set(null);
});

afterEach(() => {
  vi.unstubAllGlobals();
});

const jsonResponse = (payload: unknown, status = 200): Response =>
  new Response(JSON.stringify(payload), {
    status,
    headers: { "Content-Type": "application/json" },
  });

async function setToken(token: string): Promise<void> {
  const { setAgentToken } = await import("../stores/agentApiToken");
  setAgentToken(token);
}

function lastCall(): { url: string; init: RequestInit } {
  expect(fetchMock).toHaveBeenCalled();
  const calls = fetchMock.mock.calls as [string, RequestInit][];
  const [url, init] = calls[calls.length - 1];
  return { url, init };
}

function lastBody(): Record<string, unknown> {
  return JSON.parse(String(lastCall().init.body)) as Record<string, unknown>;
}

describe("config verb client", () => {
  it("getConfigState GETs /agent/v1/config and returns the safe state", async () => {
    await setToken("s3cret");
    const { getConfigState } = await import("./agentApi");
    const payload = {
      peers: [{ name: "upstream", proxy: "http://x", apiKey: { macro: "${env.K}", set: true }, tailcatKey: { set: false }, models: ["m"] }],
      selectors: [],
      agentApi: { enabled: true, key: { set: true } },
    };
    fetchMock.mockResolvedValueOnce(jsonResponse(payload));

    const state = await getConfigState();
    expect(state.peers[0].apiKey.macro).toBe("${env.K}");
    const { url, init } = lastCall();
    expect(url).toBe("/agent/v1/config");
    expect(init.method).toBe("GET");
  });

  it("peers.add sends the typed payload with approved=false for the dry run", async () => {
    await setToken("s3cret");
    const { addPeer } = await import("./agentApi");
    fetchMock.mockResolvedValueOnce(jsonResponse({ status: "pending", approved: false, written: false, reloadRequired: true }));

    const res = await addPeer({
      approved: false,
      name: "openrouter",
      proxy: "https://openrouter.ai/api",
      apiKey: "${env.PEER_KEY}",
      models: ["llama-3.1-8b"],
    });

    expect(res.status).toBe("pending");
    const { url } = lastCall();
    expect(url).toBe("/agent/v1/config/peers.add");
    expect(lastBody()).toEqual({
      approved: false,
      name: "openrouter",
      proxy: "https://openrouter.ai/api",
      apiKey: "${env.PEER_KEY}",
      models: ["llama-3.1-8b"],
    });
  });

  it("updatePeer omits untouched fields (undefined never reaches the wire)", async () => {
    await setToken("s3cret");
    const { updatePeer } = await import("./agentApi");
    fetchMock.mockResolvedValueOnce(jsonResponse({ status: "applied", approved: true, written: true, reloadRequired: false }));

    await updatePeer({ approved: true, name: "upstream", proxy: "http://new" });

    expect(lastBody()).toEqual({ approved: true, name: "upstream", proxy: "http://new" });
  });

  it("removePeer posts the name and the approval", async () => {
    await setToken("s3cret");
    const { removePeer } = await import("./agentApi");
    fetchMock.mockResolvedValueOnce(jsonResponse({ status: "applied", approved: true, written: true, reloadRequired: false }));

    await removePeer("upstream", true);

    expect(lastCall().url).toBe("/agent/v1/config/peers.remove");
    expect(lastBody()).toEqual({ name: "upstream", approved: true });
  });

  it("addSelector posts strategy, targets and cooldown", async () => {
    await setToken("s3cret");
    const { addSelector } = await import("./agentApi");
    fetchMock.mockResolvedValueOnce(jsonResponse({ status: "pending", approved: false, written: false, reloadRequired: true }));

    await addSelector({
      approved: false,
      name: "chain",
      strategy: "failover",
      targets: ["existing", "upstream/remote-model"],
      cooldown: "45s",
    });

    expect(lastCall().url).toBe("/agent/v1/config/selectors.add");
    expect(lastBody()).toEqual({
      approved: false,
      name: "chain",
      strategy: "failover",
      targets: ["existing", "upstream/remote-model"],
      cooldown: "45s",
    });
  });

  it("updateSelector and removeSelector hit their verbs", async () => {
    await setToken("s3cret");
    const { updateSelector, removeSelector } = await import("./agentApi");
    fetchMock.mockImplementation(() =>
      Promise.resolve(jsonResponse({ status: "applied", approved: true, written: true, reloadRequired: false })),
    );

    await updateSelector({ approved: true, name: "chain", targets: ["b", "a"] });
    expect(lastCall().url).toBe("/agent/v1/config/selectors.update");

    await removeSelector("chain", true);
    expect(lastCall().url).toBe("/agent/v1/config/selectors.remove");
    expect(lastBody()).toEqual({ name: "chain", approved: true });
  });

  it("updateAgentAPI posts enabled and/or key", async () => {
    await setToken("s3cret");
    const { updateAgentAPI } = await import("./agentApi");
    fetchMock.mockResolvedValueOnce(jsonResponse({ status: "applied", approved: true, written: true, reloadRequired: false }));

    await updateAgentAPI({ approved: true, key: "${env.NEW_AGENT_KEY}" });

    expect(lastCall().url).toBe("/agent/v1/config/agent_api.update");
    expect(lastBody()).toEqual({ approved: true, key: "${env.NEW_AGENT_KEY}" });
  });

  it("listConfigBackups unwraps the backups array", async () => {
    await setToken("s3cret");
    const { listConfigBackups } = await import("./agentApi");
    fetchMock.mockResolvedValueOnce(
      jsonResponse({
        backups: [
          { name: "config.yaml.bak-20261022T071053Z", path: "/x/config.yaml.bak-20261022T071053Z", sizeBytes: 740, modifiedAt: "2026-10-22T07:10:53Z" },
        ],
      }),
    );

    const backups = await listConfigBackups();
    expect(backups).toHaveLength(1);
    expect(backups[0].name).toBe("config.yaml.bak-20261022T071053Z");
    expect(lastCall().url).toBe("/agent/v1/config/backups");
  });

  it("restoreConfigBackup posts the backup name and approval", async () => {
    await setToken("s3cret");
    const { restoreConfigBackup } = await import("./agentApi");
    fetchMock.mockResolvedValueOnce(
      jsonResponse({ status: "applied", approved: true, written: true, reloadRequired: false, restoredFrom: "/x/config.yaml.bak-20261022T071053Z" }),
    );

    const res = await restoreConfigBackup("config.yaml.bak-20261022T071053Z", true);

    expect(lastCall().url).toBe("/agent/v1/config/restore");
    expect(lastBody()).toEqual({ backup: "config.yaml.bak-20261022T071053Z", approved: true });
    expect(res.restoredFrom).toContain("config.yaml.bak-20261022T071053Z");
  });

  it("surfaces the validator's exact message on a 400", async () => {
    await setToken("s3cret");
    const { addPeer, AgentApiError } = await import("./agentApi");
    fetchMock.mockResolvedValueOnce(
      jsonResponse({ error: { message: 'selectors.cascade.targets[1] references unknown model "upstream/remote-model"' } }, 400),
    );

    await expect(
      addPeer({ approved: true, name: "x", proxy: "http://x", models: ["m"] }),
    ).rejects.toMatchObject({
      name: "AgentApiError",
      status: 400,
      message: expect.stringContaining("references unknown model"),
    });
    expect(AgentApiError).toBeDefined();
  });

  it("rejects with AgentUnauthorizedError on 401 without a stored token", async () => {
    const { getConfigState, AgentUnauthorizedError } = await import("./agentApi");
    await expect(getConfigState()).rejects.toBeInstanceOf(AgentUnauthorizedError);
    expect(fetchMock).not.toHaveBeenCalled();
  });
});
