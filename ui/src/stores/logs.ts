import { writable, type Writable } from "svelte/store";
import type { APIEventEnvelope, LogData, LogSource } from "../lib/types";
import { apiUrl } from "../lib/apiPath";
import { apiAuthHeaders } from "../lib/apiAuth";
import { openSSE, type SSEConnection } from "../lib/sseStream";

const LOG_LENGTH_LIMIT = 1024 * 100; /* 100KB of log data */

export const proxyLogs = writable<string>("");
export const upstreamLogs = writable<string>("");
export const httpLogs = writable<string>("");

const logStores: Record<LogSource, Writable<string>> = {
  proxy: proxyLogs,
  upstream: upstreamLogs,
  http: httpLogs,
};

function appendLog(newData: string, store: Writable<string>): void {
  store.update((prev) => {
    const updatedLog = prev + newData;
    return updatedLog.length > LOG_LENGTH_LIMIT ? updatedLog.slice(-LOG_LENGTH_LIMIT) : updatedLog;
  });
}

export function handleLogEventMessage(data: string): void {
  const message = JSON.parse(data) as APIEventEnvelope;
  if (message.type !== "logData") return;
  const logData = JSON.parse(message.data) as LogData;
  const store = logStores[logData.source];
  if (store) appendLog(logData.data, store);
}

/**
 * Stream the given log sources from GET /api/events/logs into their stores
 * (proxyLogs, upstreamLogs, httpLogs). The server sends each stream's history
 * first, so the stores are cleared whenever the connection (re)opens. The
 * connection retries with backoff until the returned function is called,
 * which closes it and clears the stores.
 */
export function connectLogStreams(sources: LogSource[]): () => void {
  const query = new URLSearchParams();
  for (const source of sources) query.append("stream", source);
  const url = `/api/events/logs?${query}`;
  const clear = () => sources.forEach((source) => logStores[source].set(""));

  let eventStream: SSEConnection | null = null;
  let retryTimer: ReturnType<typeof setTimeout> | null = null;
  let retryCount = 0;
  let closed = false;
  const initialDelay = 1000; // 1 second

  const connect = () => {
    retryTimer = null;
    // EventSource cannot attach the Authorization header the apiKeys
    // config needs, so the log stream uses the fetch-based SSE client.
    eventStream = openSSE(
      apiUrl(url),
      {
        onopen: () => {
          clear();
          retryCount = 0;
        },
        onmessage: (data: string) => {
          try {
            handleLogEventMessage(data);
          } catch (err) {
            console.error(data, err);
          }
        },
        onerror: () => {
          eventStream = null;
          if (closed) return;
          retryCount++;
          const delay = Math.min(initialDelay * Math.pow(2, retryCount - 1), 5000);
          retryTimer = setTimeout(connect, delay);
        },
      },
      apiAuthHeaders(),
    );
  };

  connect();

  return () => {
    closed = true;
    if (retryTimer) clearTimeout(retryTimer);
    eventStream?.close();
    eventStream = null;
    clear();
  };
}
