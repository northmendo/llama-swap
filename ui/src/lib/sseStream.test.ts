import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

/**
 * Tests for the fetch-based SSE client the UI uses on endpoints that
 * need an Authorization header (which the browser's EventSource cannot
 * send): frame parsing, header attachment, the 401 token prompt and
 * close semantics.
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

/** A streamed 200 response body carrying the given raw chunks. */
function sseResponse(chunks: string[]): Response {
  const body = new ReadableStream<Uint8Array>({
    start(controller) {
      const encoder = new TextEncoder();
      for (const chunk of chunks) controller.enqueue(encoder.encode(chunk));
      controller.close();
    },
  });
  return { ok: true, status: 200, body } as unknown as Response;
}

async function setToken(token: string): Promise<void> {
  const { setAgentToken } = await import("../stores/agentApiToken");
  setAgentToken(token);
}

describe("parseSSEBlock", () => {
  let parseSSEBlock: typeof import("./sseStream").parseSSEBlock;

  beforeEach(async () => {
    ({ parseSSEBlock } = await import("./sseStream"));
  });

  it("returns the payload of a single data line", () => {
    expect(parseSSEBlock("data: hello")).toBe("hello");
  });

  it("joins multiple data lines with newlines like EventSource", () => {
    expect(parseSSEBlock("data: line one\ndata: line two")).toBe("line one\nline two");
  });

  it("ignores comments, events and other fields", () => {
    expect(parseSSEBlock(": keep-alive\nevent: logData\nid: 7\ndata: x")).toBe("x");
  });

  it("returns null for blocks without data", () => {
    expect(parseSSEBlock(": ping")).toBeNull();
    expect(parseSSEBlock("event: only")).toBeNull();
    expect(parseSSEBlock("")).toBeNull();
  });
});

describe("openSSE", () => {
  it("delivers each frame's payload and passes the auth headers to fetch", async () => {
    await setToken("s3cret");
    const { openSSE } = await import("./sseStream");
    fetchMock.mockResolvedValueOnce(
      sseResponse(["data: one\n\n", 'data: {"a":1}\n\n', ": keep-alive\n\n", "data: two\n"]),
    );

    const messages: string[] = [];
    const opened = vi.fn();
    openSSE("/api/events", { onopen: opened, onmessage: (d) => messages.push(d) }, {
      Authorization: "Bearer s3cret",
    });
    await vi.waitFor(() => expect(messages).toEqual(["one", '{"a":1}', "two"]));

    expect(opened).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/events");
    expect((init.headers as Record<string, string>).Authorization).toBe("Bearer s3cret");
  });

  it("reports a non-2xx response through onerror", async () => {
    const { openSSE } = await import("./sseStream");
    fetchMock.mockResolvedValueOnce(new Response("nope", { status: 500 }));

    const errored = vi.fn();
    openSSE("/api/events", { onmessage: () => {}, onerror: errored });
    await vi.waitFor(() => expect(errored).toHaveBeenCalledTimes(1));
  });

  it("prompts for the token on a 401 stream", async () => {
    const { openSSE } = await import("./sseStream");
    const { apiAuthPrompt } = await import("./apiAuth");
    fetchMock.mockResolvedValueOnce(new Response("unauthorized", { status: 401 }));
    let prompted: string | null = null;
    apiAuthPrompt.subscribe((reason) => (prompted = reason));

    const errored = vi.fn();
    openSSE("/api/events", { onmessage: () => {}, onerror: errored });

    await vi.waitFor(() => expect(errored).toHaveBeenCalledTimes(1));
    expect(prompted).toMatch(/apiKeys/);
  });

  it("does not fire onerror after close()", async () => {
    const { openSSE } = await import("./sseStream");
    // A stream that stays open forever; only close() ends it.
    const body = new ReadableStream<Uint8Array>({
      start(controller) {
        const encoder = new TextEncoder();
        controller.enqueue(encoder.encode("data: one\n\n"));
        // never closed: the reader stays pending until close() aborts it
      },
    });
    fetchMock.mockResolvedValueOnce({ ok: true, status: 200, body } as unknown as Response);

    const messages: string[] = [];
    const errored = vi.fn();
    const conn = openSSE("/api/events", { onmessage: (d) => messages.push(d), onerror: errored });
    await vi.waitFor(() => expect(messages).toEqual(["one"]));

    conn.close();
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(errored).not.toHaveBeenCalled();
  });
});
