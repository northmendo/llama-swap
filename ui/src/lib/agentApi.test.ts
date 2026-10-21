import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

/**
 * Tests for the /agent/v1 typed client. fetch is mocked; the token is
 * driven through the real store (in-memory, since these tests run in
 * node where localStorage does not exist — persistence is covered by
 * stores/agentApiToken.test.ts).
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

/** The last fetch call the client should have made. */
function lastCall(): { url: string; init: RequestInit } {
  expect(fetchMock).toHaveBeenCalled();
  const calls = fetchMock.mock.calls as [string, RequestInit][];
  const [url, init] = calls[calls.length - 1];
  return { url, init };
}

describe("agent client auth", () => {
  it("refuses to call the API without a stored token", async () => {
    const { listEngines, AgentUnauthorizedError } = await import("./agentApi");
    await expect(listEngines()).rejects.toBeInstanceOf(AgentUnauthorizedError);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("sends the bearer token and the JSON content type on POSTs", async () => {
    await setToken("s3cret");
    const { listEngines } = await import("./agentApi");
    fetchMock.mockResolvedValueOnce(jsonResponse({ engines: [] }));

    await listEngines();

    const { url, init } = lastCall();
    expect(url).toBe("/agent/v1/engines");
    expect(init.method).toBe("GET");
    expect((init.headers as Record<string, string>).Authorization).toBe("Bearer s3cret");
  });

  it("surfaces a 401 as AgentUnauthorizedError with the server's message", async () => {
    await setToken("wrong");
    const { listEngines, AgentUnauthorizedError } = await import("./agentApi");
    fetchMock.mockResolvedValueOnce(
      jsonResponse({ error: { message: "unauthorized: missing or invalid bearer token" } }, 401),
    );

    const err: unknown = await listEngines().catch((e: unknown) => e);
    expect(err).toBeInstanceOf(AgentUnauthorizedError);
    expect((err as InstanceType<typeof AgentUnauthorizedError>).status).toBe(401);
    expect((err as Error).message).toContain("unauthorized");
  });

  it("maps other errors to AgentApiError with the envelope message and status", async () => {
    await setToken("s3cret");
    const { scanModels, AgentApiError } = await import("./agentApi");
    fetchMock.mockResolvedValueOnce(
      jsonResponse({ error: { message: "discovery is not configured" } }, 400),
    );

    const err: unknown = await scanModels().catch((e: unknown) => e);
    expect(err).toBeInstanceOf(AgentApiError);
    expect((err as InstanceType<typeof AgentApiError>).status).toBe(400);
    expect((err as Error).message).toBe("discovery is not configured");
  });

  it("falls back to a status-only message when the error body is not JSON", async () => {
    await setToken("s3cret");
    const { listEngines, AgentApiError } = await import("./agentApi");
    fetchMock.mockResolvedValueOnce(new Response("gateway exploded", { status: 502 }));

    const err: unknown = await listEngines().catch((e: unknown) => e);
    expect(err).toBeInstanceOf(AgentApiError);
    expect((err as InstanceType<typeof AgentApiError>).status).toBe(502);
    expect((err as Error).message).toContain("502");
  });
});

describe("agent client endpoints", () => {
  it("listEngines reads GET /agent/v1/engines", async () => {
    await setToken("k");
    const { listEngines } = await import("./agentApi");
    fetchMock.mockResolvedValueOnce(
      jsonResponse({ engines: [{ name: "beellama", label: "b12", cmd: "/opt/llama-server", envKeys: ["FOO"] }] }),
    );

    const engines = await listEngines();
    expect(engines).toEqual([{ name: "beellama", label: "b12", cmd: "/opt/llama-server", envKeys: ["FOO"] }]);
    expect(lastCall().url).toBe("/agent/v1/engines");
    expect(lastCall().init.method).toBe("GET");
  });

  it("listEngineUpdates reads GET /agent/v1/engines/updates", async () => {
    await setToken("k");
    const { listEngineUpdates } = await import("./agentApi");
    fetchMock.mockResolvedValueOnce(
      jsonResponse({
        engines: [{ name: "beellama", hasUpdateSource: true, updateAvailable: { tag: "b13", checkedAt: "x" } }],
      }),
    );

    const updates = await listEngineUpdates();
    expect(updates[0].updateAvailable?.tag).toBe("b13");
    expect(lastCall().url).toBe("/agent/v1/engines/updates");
  });

  it("checkEngineUpdates posts the engines filter (empty = check all)", async () => {
    await setToken("k");
    const { checkEngineUpdates } = await import("./agentApi");
    fetchMock.mockResolvedValueOnce(jsonResponse({ results: [] }));

    await checkEngineUpdates();
    expect(lastCall().url).toBe("/agent/v1/engines/check");
    expect(lastCall().init.method).toBe("POST");
    expect(JSON.parse(lastCall().init.body as string)).toEqual({ engines: [] });

    fetchMock.mockResolvedValueOnce(jsonResponse({ results: [{ engine: "beellama", updateAvailable: true }] }));
    const results = await checkEngineUpdates(["beellama"]);
    expect(JSON.parse(lastCall().init.body as string)).toEqual({ engines: ["beellama"] });
    expect(results[0].engine).toBe("beellama");
  });

  it("updateEngine posts approved and force to the engine's update route", async () => {
    await setToken("k");
    const { updateEngine } = await import("./agentApi");
    fetchMock.mockResolvedValueOnce(jsonResponse({ engine: "beellama", approved: false }));

    await updateEngine("beellama", { approved: false });
    expect(lastCall().url).toBe("/agent/v1/engines/beellama/update");
    expect(JSON.parse(lastCall().init.body as string)).toEqual({ approved: false, force: false });

    fetchMock.mockResolvedValueOnce(
      jsonResponse({ engine: "beellama", approved: true, installedVersion: "b13", stoppedModels: ["saluki"] }),
    );
    const res = await updateEngine("beellama", { approved: true, force: true });
    expect(JSON.parse(lastCall().init.body as string)).toEqual({ approved: true, force: true });
    expect(res.stoppedModels).toEqual(["saluki"]);
  });

  it("URL-encodes engine names in path segments", async () => {
    await setToken("k");
    const { rollbackEngine } = await import("./agentApi");
    fetchMock.mockResolvedValueOnce(jsonResponse({ engine: "a/b", approved: true }));

    await rollbackEngine("a/b", { approved: true });
    expect(lastCall().url).toBe("/agent/v1/engines/a%2Fb/rollback");
  });

  it("rollbackEngine posts approved and force to the rollback route", async () => {
    await setToken("k");
    const { rollbackEngine } = await import("./agentApi");
    fetchMock.mockResolvedValueOnce(
      jsonResponse({ engine: "beellama", approved: true, restoredFrom: "/opt/x.prev" }),
    );

    const res = await rollbackEngine("beellama", { approved: true, force: false });
    expect(lastCall().url).toBe("/agent/v1/engines/beellama/rollback");
    expect(lastCall().init.method).toBe("POST");
    expect(JSON.parse(lastCall().init.body as string)).toEqual({ approved: true, force: false });
    expect(res.restoredFrom).toBe("/opt/x.prev");
  });

  it("scanModels posts an empty body to /agent/v1/scan", async () => {
    await setToken("k");
    const { scanModels } = await import("./agentApi");
    fetchMock.mockResolvedValueOnce(jsonResponse({ found: 2, drafted: 1, skipped: 1, results: [] }));

    const scan = await scanModels();
    expect(lastCall().url).toBe("/agent/v1/scan");
    expect(JSON.parse(lastCall().init.body as string)).toEqual({});
    expect(scan.found).toBe(2);
  });

  it("validateYaml posts the draft yaml", async () => {
    await setToken("k");
    const { validateYaml } = await import("./agentApi");
    fetchMock.mockResolvedValueOnce(jsonResponse({ valid: false, errors: ["boom"] }));

    const res = await validateYaml("saluki:\n  cmd: x");
    expect(lastCall().url).toBe("/agent/v1/validate");
    expect(JSON.parse(lastCall().init.body as string)).toEqual({ yaml: "saluki:\n  cmd: x" });
    expect(res.valid).toBe(false);
    expect(res.errors).toEqual(["boom"]);
  });

  it("applyModel posts the yaml and the approval", async () => {
    await setToken("k");
    const { applyModel } = await import("./agentApi");
    fetchMock.mockResolvedValueOnce(
      jsonResponse({ status: "applied", model: "saluki", written: true, reloadRequired: true, backup: "cfg.bak" }),
    );

    const res = await applyModel("saluki:\n  cmd: x", true);
    expect(lastCall().url).toBe("/agent/v1/apply");
    expect(JSON.parse(lastCall().init.body as string)).toEqual({ yaml: "saluki:\n  cmd: x", approved: true });
    expect(res.reloadRequired).toBe(true);
    expect(res.backup).toBe("cfg.bak");
  });

  it("refineDraft posts the model file path", async () => {
    await setToken("k");
    const { refineDraft } = await import("./agentApi");
    fetchMock.mockResolvedValueOnce(
      jsonResponse({ modelId: "saluki", engine: "beellama", args: "", ctxSize: 8192, ctxCapped: false, yaml: "x", refined: true }),
    );

    const res = await refineDraft("/models/saluki.gguf");
    expect(lastCall().url).toBe("/agent/v1/refine");
    expect(JSON.parse(lastCall().init.body as string)).toEqual({ path: "/models/saluki.gguf" });
    expect(res.refined).toBe(true);
  });
});
