import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

/**
 * Tests for the shared auth helpers llama-swap's own API uses when the
 * config sets `apiKeys`. fetch is mocked; the token is driven through
 * the real store (in-memory, since these tests run in node where
 * localStorage does not exist — persistence is covered by
 * stores/agentApiToken.test.ts).
 */

let fetchMock: ReturnType<typeof vi.fn>;

beforeEach(async () => {
  fetchMock = vi.fn();
  vi.stubGlobal("fetch", fetchMock);
  vi.resetModules();

  const token = await import("../stores/agentApiToken");
  token.agentToken.set(null);

  const { apiAuthPrompt } = await import("./apiAuth");
  apiAuthPrompt.set(null);
});

afterEach(() => {
  vi.unstubAllGlobals();
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

const jsonResponse = (status = 200): Response =>
  new Response("{}", { status, headers: { "Content-Type": "application/json" } });

describe("apiAuthHeaders", () => {
  it("adds the stored token's Authorization header onto the base headers", async () => {
    await setToken("s3cret");
    const { apiAuthHeaders } = await import("./apiAuth");

    const headers = apiAuthHeaders({ "Content-Type": "application/json" });

    expect(headers.Authorization).toBe("Bearer s3cret");
    expect(headers["Content-Type"]).toBe("application/json");
  });

  it("leaves the headers untouched when no token is stored", async () => {
    const { apiAuthHeaders } = await import("./apiAuth");
    const base = { "Content-Type": "application/json" };

    expect(apiAuthHeaders(base)).toBe(base);
    expect(apiAuthHeaders(base).Authorization).toBeUndefined();
  });
});

describe("authedFetch", () => {
  it("sends the stored bearer token on the request", async () => {
    await setToken("s3cret");
    const { authedFetch } = await import("./apiAuth");
    fetchMock.mockResolvedValueOnce(jsonResponse());

    await authedFetch("/v1/models");

    const { url, init } = lastCall();
    expect(url).toBe("/v1/models");
    expect((init.headers as Record<string, string>).Authorization).toBe("Bearer s3cret");
  });

  it("goes out keyless when no token is stored, exactly like before", async () => {
    const { authedFetch } = await import("./apiAuth");
    fetchMock.mockResolvedValueOnce(jsonResponse());

    const response = await authedFetch("/v1/models");

    expect(response.status).toBe(200);
    expect((lastCall().init.headers as Record<string, string>).Authorization).toBeUndefined();
  });

  it("surfaces a 401 as the token prompt instead of a bare failure", async () => {
    const { authedFetch, apiAuthPrompt } = await import("./apiAuth");
    fetchMock.mockResolvedValueOnce(jsonResponse(401));
    let prompted: string | null = null;
    apiAuthPrompt.subscribe((reason) => (prompted = reason));

    const response = await authedFetch("/v1/models");

    // The response still reaches the caller untouched.
    expect(response.status).toBe(401);
    // ...and the missing-token path opened the dialog prompt.
    expect(prompted).toMatch(/apiKeys/);
  });

  it("does not prompt on non-401 failures", async () => {
    const { authedFetch, apiAuthPrompt } = await import("./apiAuth");
    fetchMock.mockResolvedValueOnce(jsonResponse(500));
    let prompted: string | null = null;
    apiAuthPrompt.subscribe((reason) => (prompted = reason));

    await authedFetch("/v1/models");

    // The subscription fires once with the current (null) value; a prompt
    // would have replaced it with the reason string.
    expect(prompted).toBeNull();
  });
});
