/**
 * SSE over fetch: text/event-stream frames read from a fetch response
 * body, for endpoints that need an Authorization header. The browser's
 * EventSource cannot send headers, so the UI's gated event streams
 * (/api/events, /api/events/logs) come through here instead of
 * EventSource.
 *
 * Only the EventSource subset the UI uses is implemented: data
 * payloads, open and error callbacks. Reconnection is left to the
 * callers, which already own backoff loops.
 */

import { promptForApiToken } from "./apiAuth";

/**
 * Extract the joined data payload of one event-stream frame block, the
 * way EventSource does: `data:` lines (one leading space stripped) are
 * joined with newlines, comments and other fields are ignored. Returns
 * null for blocks carrying no data.
 */
export function parseSSEBlock(block: string): string | null {
  const dataLines: string[] = [];
  for (const rawLine of block.split("\n")) {
    const line = rawLine.replace(/\r$/, "");
    if (!line || line.startsWith(":")) continue;
    if (line.startsWith("data:")) {
      dataLines.push(line.slice(5).replace(/^ /, ""));
    }
  }
  return dataLines.length ? dataLines.join("\n") : null;
}

/** A live event-stream connection; close() stops it for good. */
export interface SSEConnection {
  close(): void;
}

/**
 * Opens url and dispatches each event's data payload to handlers.onmessage.
 * onopen fires when the stream is established; onerror fires on any
 * transport failure or when the server ends the stream — never after
 * close(), so callers can treat it as "reconnect, unless I closed it".
 * A 401 additionally surfaces the token prompt (see apiAuth).
 */
export function openSSE(
  url: string,
  handlers: {
    onopen?: () => void;
    onmessage: (data: string) => void;
    onerror?: () => void;
  },
  headers: Record<string, string> = {},
): SSEConnection {
  const controller = new AbortController();
  let closed = false;

  const fail = () => {
    if (!closed) handlers.onerror?.();
  };

  void (async () => {
    let response: Response;
    try {
      response = await fetch(url, { headers, signal: controller.signal });
    } catch {
      fail();
      return;
    }
    if (closed) return;
    if (!response.ok || !response.body) {
      if (response.status === 401) {
        promptForApiToken(
          "llama-swap rejected the live event stream: its API key (the apiKeys list in the config) is missing or wrong. Enter it once; it is stored in this browser only.",
        );
      }
      fail();
      return;
    }

    handlers.onopen?.();

    const reader = response.body.getReader();
    const decoder = new TextDecoder();
    let buffer = "";
    try {
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, { stream: true });
        const blocks = buffer.split("\n\n");
        buffer = blocks.pop() ?? "";
        for (const block of blocks) {
          const data = parseSSEBlock(block);
          if (data !== null) handlers.onmessage(data);
        }
      }
      // Deliver a final complete block that the server ended without a
      // trailing blank line, mirroring how EventSource flushes on close.
      const tail = parseSSEBlock(buffer);
      if (tail !== null) handlers.onmessage(tail);
    } catch {
      // aborted via close(), or a transport error mid-stream
    }
    fail();
  })();

  return {
    close() {
      closed = true;
      controller.abort();
    },
  };
}
