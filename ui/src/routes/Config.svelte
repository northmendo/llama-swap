<script lang="ts">
  /**
   * Config view: everything the fork added that used to be YAML-only —
   * peers, selector cascades and the agent_api section — editable in
   * the browser. All writes go through the bearer-authenticated
   * /agent/v1 config verbs, each one a dry-run "Validate" followed by
   * an approved "Apply" that backs the config file up first:
   *
   *   GET  /agent/v1/config              safe state (keys as ${env.VAR} macros)
   *   POST /agent/v1/config/peers.*      add / update / remove a peer
   *   POST /agent/v1/config/selectors.*   add / update / remove a cascade
   *   POST /agent/v1/config/agent_api.update  enable toggle, key macro
   *   GET  /agent/v1/config/backups      timestamped backups
   *   POST /agent/v1/config/restore      put one back (approved)
   *
   * Per-target cascade health comes from /v1/models
   * (meta.llamaswap.failover), the same data the Models page shows.
   */
  import { onMount } from "svelte";
  import { Tabs, TabsContent, TabsList, TabsTrigger } from "$lib/components/ui/tabs/index.js";
  import { Button } from "$lib/components/ui/button/index.js";
  import { RefreshCw } from "@lucide/svelte";
  import AgentTokenDialog from "../components/AgentTokenDialog.svelte";
  import EmptyState from "../components/EmptyState.svelte";
  import PeersPanel from "../components/config/PeersPanel.svelte";
  import CascadesPanel from "../components/config/CascadesPanel.svelte";
  import AgentAPIPanel from "../components/config/AgentAPIPanel.svelte";
  import { apiUrl } from "../lib/apiPath";
  import { authedFetch } from "../lib/apiAuth";
  import {
    AgentUnauthorizedError,
    currentToken,
    getConfigSchema,
    getConfigState,
    getEnvNames,
    getPeerCatalogs,
    listConfigBackups,
    type ConfigBackup,
    type ConfigSchema,
    type ConfigState,
    type EnvName,
    type PeerCatalogEntry,
  } from "../lib/agentApi";
  import { failoverHealthFromModels, type SelectorMeta, type TargetHealth } from "../lib/configUi";

  let config = $state<ConfigState | null>(null);
  let backups = $state<ConfigBackup[]>([]);
  let health = $state<Map<string, TargetHealth[]>>(new Map());
  /** Model IDs this server itself serves (local models and configured
   * peer models; selectors excluded) — the local half of the target
   * pick list, from the same /v1/models payload as the health map. */
  let localModels = $state<string[]>([]);
  /** The server's cached provider catalogs: what hydrates the model pickers
   * without probing a single provider on page load. */
  let catalogs = $state<PeerCatalogEntry[]>([]);
  /** ${env.VAR} NAMES (never values) the key pickers offer. */
  let envNames = $state<EnvName[]>([]);
  /** The finite value sets from the server: strategies come from the
   * config validator itself, so the picker cannot drift. */
  let schema = $state<ConfigSchema | null>(null);
  let loading = $state(false);
  let error = $state<string | null>(null);

  let tokenOpen = $state(false);
  let tokenReason = $state("");

  function message(e: unknown): string {
    return e instanceof Error ? e.message : String(e);
  }

  function openTokenPrompt(reason: string): void {
    tokenReason = reason;
    tokenOpen = true;
  }

  /** Offer the token prompt; returns true when the error was a 401. */
  function authProblem(e: unknown): boolean {
    if (e instanceof AgentUnauthorizedError) {
      openTokenPrompt("The saved key was rejected (401). Enter the agent_api key of this server.");
      return true;
    }
    return false;
  }

  /** The failover health map from a raw /v1/models payload. */
  function healthFromModelsPayload(payload: unknown): Map<string, TargetHealth[]> {
    const records =
      (payload as { data?: Array<{ id: string; meta?: { llamaswap?: SelectorMeta } }> })?.data ?? [];
    return failoverHealthFromModels(records);
  }

  /** The pickable local model IDs from the same payload: every record
   * that is not a selector (selector chaining is invalid). */
  function localModelsFromPayload(payload: unknown): string[] {
    const records =
      (payload as { data?: Array<{ id: string; meta?: { llamaswap?: SelectorMeta } }> })?.data ?? [];
    return records.filter((r) => r.meta?.llamaswap?.type !== "selector").map((r) => r.id);
  }

  /**
   * Load the pickers' finite value sets: the cached provider catalogs, the
   * env var NAMES a key can reference, and the schema's strategy list. Each
   * is best-effort — a server that predates one of them must still render
   * the config screen, only with that picker empty.
   */
  async function loadPickers(): Promise<void> {
    try {
      catalogs = await getPeerCatalogs();
    } catch (e) {
      if (authProblem(e)) return;
      catalogs = [];
    }
    try {
      envNames = await getEnvNames();
    } catch (e) {
      if (authProblem(e)) return;
      envNames = [];
    }
    try {
      schema = await getConfigSchema();
    } catch (e) {
      if (authProblem(e)) return;
      schema = null;
    }
  }

  /** Load the config state, the backup list and the cascade health. */
  async function load(): Promise<void> {
    if (!currentToken()) {
      openTokenPrompt("");
      return;
    }
    loading = true;
    error = null;
    try {
      const [configState, backupList, modelsResp] = await Promise.all([
        getConfigState(),
        listConfigBackups(),
        authedFetch(apiUrl("/v1/models")),
      ]);
      config = configState;
      backups = backupList;
      if (modelsResp.ok) {
        const payload = await modelsResp.json();
        health = healthFromModelsPayload(payload);
        localModels = localModelsFromPayload(payload);
      } else {
        health = new Map();
        localModels = [];
      }
      await loadPickers();
    } catch (e) {
      if (!authProblem(e)) error = message(e);
    } finally {
      loading = false;
    }
  }

  onMount(load);

  /** Any successful write from a panel refreshes everything — now and
   * once more after the --watch-config reload window, because the
   * server's in-memory config (which GET /agent/v1/config reads) lags
   * the file by up to its 2-second poll interval. */
  let reloadTimer: ReturnType<typeof setTimeout> | null = null;

  function changed(): void {
    void load();
    if (reloadTimer) clearTimeout(reloadTimer);
    reloadTimer = setTimeout(() => void load(), 2500);
  }

  /** Panels surface 401s through the shared token prompt. */
  function panelAuthProblem(e: unknown): boolean {
    return authProblem(e);
  }

  /** Panels surface other errors through the shared error banner. */
  function panelError(text: string): void {
    error = text;
  }
</script>

<div class="mx-auto max-w-3xl space-y-4 p-2">
  <div class="flex flex-wrap items-center gap-2">
    <h3 class="text-lg font-semibold">Config</h3>
    <span class="text-muted-foreground text-sm">peers, cascades and the agent API — editable in the browser</span>
    <div class="ml-auto">
      <Button variant="outline" size="sm" onclick={load} disabled={loading}>
        <RefreshCw class={loading ? "animate-spin" : ""} />
        Refresh
      </Button>
    </div>
  </div>

  {#if error}
    <div class="text-destructive rounded-lg border border-destructive/40 bg-destructive/10 p-3 text-sm">
      {error}
    </div>
  {/if}

  {#if loading && !config}
    <EmptyState message="Loading config…" />
  {:else if config}
    <Tabs value="peers">
      <TabsList>
        <TabsTrigger value="peers">Peers</TabsTrigger>
        <TabsTrigger value="cascades">Cascades</TabsTrigger>
        <TabsTrigger value="agentapi">Agent API</TabsTrigger>
      </TabsList>
      <TabsContent value="peers" class="mt-4">
        <PeersPanel
          peers={config.peers}
          {catalogs}
          {envNames}
          onchanged={changed}
          onauth={panelAuthProblem}
          onerror={panelError}
          onrefresh={loadPickers}
        />
      </TabsContent>
      <TabsContent value="cascades" class="mt-4">
        <CascadesPanel
          selectors={config.selectors}
          {health}
          peers={config.peers}
          {localModels}
          {catalogs}
          strategies={schema?.selectorStrategies ?? []}
          defaultStrategy={schema?.defaultSelectorStrategy ?? "failover"}
          onchanged={changed}
          onauth={panelAuthProblem}
          onerror={panelError}
          onrefresh={loadPickers}
        />
      </TabsContent>
      <TabsContent value="agentapi" class="mt-4">
        <AgentAPIPanel
          agentApi={config.agentApi ?? null}
          {backups}
          onchanged={changed}
          onauth={panelAuthProblem}
          onerror={panelError}
        />
      </TabsContent>
    </Tabs>
  {/if}
</div>

<AgentTokenDialog
  bind:open={tokenOpen}
  reason={tokenReason}
  onsaved={() => void load()}
/>
