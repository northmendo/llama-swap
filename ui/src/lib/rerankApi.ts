import { playgroundSessionHeaders } from "./playgroundSession";
import { apiUrl } from "./apiPath";
import { apiAuthHeaders, authedFetch } from "./apiAuth";

export interface RerankResult {
  index: number;
  relevance_score: number;
}

export interface RerankResponse {
  model: string;
  object: string;
  usage: { prompt_tokens: number; total_tokens: number };
  results: RerankResult[];
}

export async function rerank(
  model: string,
  query: string,
  documents: string[],
  signal: AbortSignal
): Promise<RerankResponse> {
  const response = await authedFetch(apiUrl("/v1/rerank"), {
    method: "POST",
    headers: apiAuthHeaders({ "Content-Type": "application/json", ...playgroundSessionHeaders }),
    body: JSON.stringify({ model, query, documents }),
    signal,
  });
  if (!response.ok) throw new Error(`${response.status} ${response.statusText}`);
  return response.json();
}
