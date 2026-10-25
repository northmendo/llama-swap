import { get } from "svelte/store";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { connectLogStreams, handleLogEventMessage, httpLogs, proxyLogs, upstreamLogs } from "./logs";

function logMessage(source: string, data: string): string {
  return JSON.stringify({ type: "logData", data: JSON.stringify({ source, data }) });
}

/**
 * The log stream rides on the fetch-based SSE client (EventSource cannot
 * attach the Authorization header the apiKeys config needs), so the mock
 * is a controllable event-stream response per fetch call.
 */
class FakeSSEStream {
  static instances: FakeSSEStream[] = [];
  url = "";
  headers: Record<string, string> = {};
  aborted = false;
  private controller: ReadableStreamDefaultController<Uint8Array> | null = null;

  constructor() {
    FakeSSEStream.instances.push(this);
  }

  respond(url: string, init: RequestInit): Response {
    this.url = url;
    this.headers = (init.headers ?? {}) as Record<string, string>;
    const self = this;
    const body = new ReadableStream<Uint8Array>({
      start(controller) {
        self.controller = controller;
      },
    });
    init.signal?.addEventListener("abort", () => {
      self.aborted = true;
      try {
        self.controller?.close();
      } catch {
        // already closed
      }
    });
    return { ok: true, status: 200, body } as unknown as Response;
  }

  /** Deliver one event-stream frame. */
  send(data: string): void {
    this.controller?.enqueue(new TextEncoder().encode(`data: ${data}\n\n`));
  }

  /** End the stream, the way a server-side disconnect does. */
  end(): void {
    try {
      this.controller?.close();
    } catch {
      // already closed
    }
  }
}

/** Let pending microtasks (fetch resolution, stream reads) run. */
const flush = () => new Promise((resolve) => setTimeout(resolve, 0));

beforeEach(() => {
  FakeSSEStream.instances = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((url: string, init?: RequestInit) =>
      Promise.resolve(new FakeSSEStream().respond(url, init ?? {}))
    )
  );
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
  proxyLogs.set("");
  upstreamLogs.set("");
  httpLogs.set("");
});

describe("log streams", () => {
  it("routes log data to the store for its source", () => {
    handleLogEventMessage(logMessage("proxy", "p1\n"));
    handleLogEventMessage(logMessage("upstream", "u1\n"));
    handleLogEventMessage(logMessage("http", "h1\n"));
    handleLogEventMessage(logMessage("http", "h2\n"));

    expect(get(proxyLogs)).toBe("p1\n");
    expect(get(upstreamLogs)).toBe("u1\n");
    expect(get(httpLogs)).toBe("h1\nh2\n");
  });

  it("keeps only the newest 100KB of a stream", () => {
    handleLogEventMessage(logMessage("http", "a".repeat(100 * 1024)));
    handleLogEventMessage(logMessage("http", "END"));

    const log = get(httpLogs);
    expect(log.length).toBe(100 * 1024);
    expect(log.endsWith("END")).toBe(true);
  });

  it("connects only to the requested streams and clears them on close", async () => {
    upstreamLogs.set("untouched");
    proxyLogs.set("stale");
    const close = connectLogStreams(["proxy", "http"]);
    // The connection (and the store clear that comes with its opening)
    // is asynchronous, so the stale value is still in place here.
    expect(get(proxyLogs)).toBe("stale");
    await flush();
    expect(FakeSSEStream.instances).toHaveLength(1);
    const stream = FakeSSEStream.instances[0];
    expect(stream.url).toBe("/api/events/logs?stream=proxy&stream=http");
    expect(get(proxyLogs)).toBe("");

    stream.send(logMessage("proxy", "fresh"));
    await flush();
    expect(get(proxyLogs)).toBe("fresh");

    close();
    expect(stream.aborted).toBe(true);
    expect(get(proxyLogs)).toBe("");
    expect(get(upstreamLogs)).toBe("untouched");
  });

  it("reconnects after an error until closed", async () => {
    vi.useFakeTimers();
    const close = connectLogStreams(["upstream"]);
    await vi.advanceTimersByTimeAsync(0);
    expect(FakeSSEStream.instances).toHaveLength(1);

    FakeSSEStream.instances[0].end();
    await vi.advanceTimersByTimeAsync(0);
    await vi.advanceTimersByTimeAsync(1000);
    expect(FakeSSEStream.instances).toHaveLength(2);

    close();
    FakeSSEStream.instances[1].end();
    await vi.advanceTimersByTimeAsync(0);
    await vi.advanceTimersByTimeAsync(10000);
    expect(FakeSSEStream.instances).toHaveLength(2);
  });
});
