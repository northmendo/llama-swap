<script lang="ts">
  /**
   * Providers view: every configured peer/provider, and — on demand — which
   * models each one actually serves.
   *
   * On load it reads the server's cached catalogs (GET
   * /agent/v1/config/peers.catalog), which costs no upstream calls, so the
   * screen paints immediately with whatever was last known. "Check models"
   * on a row (or "Check all" at the top) runs the live probe (POST) for that
   * provider; a provider that is down, key-rejecting or slow answers with a
   * 502 envelope that renders inline on its own row and never blocks the
   * others.
   *
   * What a row shows: name, proxy host, the ${env.VAR} NAME of its key
   * macro (never a literal, never a resolved value) and whether that macro
   * currently resolves, plus reachability, latency, model count and the time
   * of the last check.
   */
  import { onMount } from "svelte";
  import { Button } from "$lib/components/ui/button/index.js";
  import { Input } from "$lib/components/ui/input/index.js";
  import { Badge } from "$lib/components/ui/badge/index.js";
  import {
    RefreshCw,
    ChevronRight,
    ChevronDown,
    CircleAlert,
    CircleCheck,
    Copy,
    KeyRound,
  } from "@lucide/svelte";
  import AgentTokenDialog from "../components/AgentTokenDialog.svelte";
  import EmptyState from "../components/EmptyState.svelte";
  import {
    AgentUnauthorizedError,
    currentToken,
    getPeerCatalogs,
    probePeerCatalog,
    type PeerCatalogEntry,
  } from "../lib/agentApi";
  import { filterPickOptions, type ModelPickOption } from "../lib/configUi";

  let {
    /** Rows to paint before the cache is read (SSR tests, and the brief
     * moment before the first GET resolves). onMount replaces them with the
     * server's cached catalogs. */
    initialPeers = [],
  }: {
    initialPeers?: PeerCatalogEntry[];
  } = $props();

  // Initial-only by design: the props are a first paint, and load() replaces
  // them with the server's cache as soon as it answers.
  // svelte-ignore state_referenced_locally
  let peers = $state<PeerCatalogEntry[]>(initialPeers);
  let loading = $state(false);
  let error = $state<string | null>(null);

  let tokenOpen = $state(false);
  let tokenReason = $state("");

  /** Per-peer transient state: a check in flight, its error, or its model
   * list expanded. One peer's failure lives here and nowhere else. */
  let checking = $state<Record<string, boolean>>({});
  let rowError = $state<Record<string, string>>({});
  let expanded = $state<Record<string, boolean>>({});
  let filters = $state<Record<string, string>>({});
  let copied = $state<string>("");

  let checkingAll = $state(false);

  function message(e: unknown): string {
    return e instanceof Error ? e.message : String(e);
  }

  function authProblem(e: unknown): boolean {
    if (e instanceof AgentUnauthorizedError) {
      tokenReason = "The saved key was rejected (401). Enter the agent_api key of this server.";
      tokenOpen = true;
      return true;
    }
    return false;
  }

  async function load(): Promise<void> {
    if (!currentToken()) {
      tokenOpen = true;
      return;
    }
    loading = true;
    error = null;
    try {
      peers = await getPeerCatalogs();
    } catch (e) {
      if (!authProblem(e)) error = message(e);
    } finally {
      loading = false;
    }
  }

  onMount(load);

  /** One provider's live check. Its own error stays on its own row. */
  async function check(name: string): Promise<boolean> {
    checking = { ...checking, [name]: true };
    const nextErrors = { ...rowError };
    delete nextErrors[name];
    rowError = nextErrors;
    try {
      await probePeerCatalog({ name });
      return true;
    } catch (e) {
      if (authProblem(e)) return false;
      rowError = { ...rowError, [name]: message(e) };
      return false;
    } finally {
      checking = { ...checking, [name]: false };
    }
  }

  async function checkOne(name: string): Promise<void> {
    await check(name);
    await load();
  }

  /** Check every provider, one at a time, so one slow host cannot hide the
   * others' results and the server is never hammered in parallel. */
  async function checkAll(): Promise<void> {
    checkingAll = true;
    try {
      for (const peer of peers) {
        await check(peer.name);
      }
      await load();
    } finally {
      checkingAll = false;
    }
  }

  function toggle(name: string): void {
    expanded = { ...expanded, [name]: !expanded[name] };
  }

  function filterFor(name: string): string {
    return filters[name] ?? "";
  }

  function modelsFor(peer: PeerCatalogEntry): ModelPickOption[] {
    const all = peer.models.map((model) => ({ id: model.id, hint: model.ownedBy }));
    return filterPickOptions(all, filterFor(peer.name));
  }

  async function copyId(id: string): Promise<void> {
    if (typeof navigator === "undefined" || !navigator.clipboard) return;
    try {
      await navigator.clipboard.writeText(id);
      copied = id;
      setTimeout(() => {
        if (copied === id) copied = "";
      }, 1500);
    } catch {
      // Clipboard permission is a browser affair; the ID stays selectable.
    }
  }

  function hostOf(proxy: string): string {
    try {
      const url = new URL(proxy);
      return url.host + (url.pathname !== "/" ? url.pathname.replace(/\/$/, "") : "");
    } catch {
      return proxy;
    }
  }

  function checkedLabel(peer: PeerCatalogEntry): string {
    if (!peer.checkedAt) return "never checked";
    const when = new Date(peer.checkedAt);
    if (Number.isNaN(when.getTime())) return peer.checkedAt;
    return when.toLocaleString();
  }

  function statusOf(peer: PeerCatalogEntry): "ok" | "error" | "unknown" {
    if (rowError[peer.name]) return "error";
    if (peer.checkedAt) return "ok";
    return "unknown";
  }
</script>

<div class="mx-auto max-w-4xl space-y-4 p-2">
  <div class="flex flex-wrap items-center gap-2">
    <h3 class="text-lg font-semibold">Providers</h3>
    <span class="text-muted-foreground text-sm">
      which models each configured provider actually serves
    </span>
    <div class="ml-auto flex gap-2">
      <Button variant="outline" size="sm" onclick={load} disabled={loading || checkingAll}>
        <RefreshCw class={loading ? "animate-spin" : ""} />
        Refresh
      </Button>
      <Button size="sm" disabled={checkingAll || peers.length === 0} onclick={() => void checkAll()}>
        <RefreshCw class={checkingAll ? "animate-spin" : ""} />
        {checkingAll ? "Checking…" : "Check all"}
      </Button>
    </div>
  </div>

  {#if error}
    <div class="text-destructive rounded-lg border border-destructive/40 bg-destructive/10 p-3 text-sm">
      {error}
    </div>
  {/if}

  {#if loading && peers.length === 0}
    <EmptyState message="Loading providers…" />
  {:else if peers.length === 0}
    <EmptyState
      message="No peers are defined in the config (peers: section). Add one on the Config screen first."
    />
  {:else}
    <div class="space-y-3">
      {#each peers as peer (peer.name)}
        {@const status = statusOf(peer)}
        <div class="rounded-lg border">
          <div class="flex flex-wrap items-center gap-2 p-3">
            <Button
              variant="ghost"
              size="sm"
              onclick={() => toggle(peer.name)}
              aria-label={`${expanded[peer.name] ? "Hide" : "Show"} models of ${peer.name}`}
            >
              {#if expanded[peer.name]}
                <ChevronDown />
              {:else}
                <ChevronRight />
              {/if}
            </Button>
            <span class="font-mono text-sm font-semibold">{peer.name}</span>

            {#if status === "ok"}
              <Badge variant="outline" class="border-success/40 bg-success/10 text-success">
                <CircleCheck class="size-3" />
                reachable
              </Badge>
            {:else if status === "error"}
              <Badge variant="outline" class="border-destructive/40 bg-destructive/10 text-destructive">
                <CircleAlert class="size-3" />
                error
              </Badge>
            {:else}
              <Badge variant="outline">unknown</Badge>
            {/if}

            <span class="text-muted-foreground font-mono text-xs break-all">
              {hostOf(peer.proxy)}
            </span>

            <div class="ml-auto flex items-center gap-2">
              <span class="text-muted-foreground text-xs">
                {peer.modelCount} model{peer.modelCount === 1 ? "" : "s"}
                {#if peer.latencyMs !== undefined}
                  · {peer.latencyMs} ms
                {/if}
                · checked {checkedLabel(peer)}
                {#if peer.truncated && peer.totalModels}
                  · showing {peer.models.length} of {peer.totalModels}
                {/if}
              </span>
              <Button
                variant="outline"
                size="sm"
                disabled={checking[peer.name] || checkingAll}
                onclick={() => void checkOne(peer.name)}
              >
                <RefreshCw class={checking[peer.name] ? "animate-spin" : ""} />
                {checking[peer.name] ? "Checking…" : "Check models"}
              </Button>
            </div>
          </div>

          <div class="flex flex-wrap items-center gap-2 border-t px-3 py-2 text-xs">
            {#if peer.keyMacro}
              <span class="text-muted-foreground inline-flex items-center gap-1">
                <KeyRound class="size-3" />
                key from
                <span class="font-mono">{peer.keyMacro}</span>
              </span>
              {#if peer.keyMacroAvailable === false}
                <Badge variant="outline" class="border-destructive/40 bg-destructive/10 text-destructive">
                  macro not resolvable — the provider will reject every request
                </Badge>
              {:else if peer.keyMacroAvailable === true}
                <Badge variant="outline" class="border-success/40 bg-success/10 text-success">
                  macro available
                </Badge>
              {/if}
            {:else}
              <span class="text-muted-foreground">no key configured</span>
            {/if}
          </div>

          {#if rowError[peer.name]}
            <div class="text-destructive border-t border-destructive/30 bg-destructive/10 px-3 py-2 text-xs">
              {rowError[peer.name]}
            </div>
          {/if}

          {#if expanded[peer.name]}
            <div class="space-y-2 border-t p-3">
              {#if peer.models.length === 0}
                <p class="text-muted-foreground text-xs">
                  No catalog cached for this provider yet
                  {#if peer.modelCount > 0}
                    ({peer.modelCount} from the config)
                  {/if}
                  — press Check models.
                </p>
              {:else}
                <Input
                  class="text-xs"
                  placeholder="Filter models…"
                  aria-label={`Filter models of ${peer.name}`}
                  value={filterFor(peer.name)}
                  oninput={(e) => (filters = { ...filters, [peer.name]: e.currentTarget.value })}
                />
                {@const shown = modelsFor(peer)}
                {#if shown.length === 0}
                  <p class="text-muted-foreground text-xs">No model matches that filter.</p>
                {:else}
                  <ul class="max-h-72 space-y-0.5 overflow-y-auto">
                    {#each shown as model (model.id)}
                      <li>
                        <button
                          type="button"
                          class="hover:bg-accent flex w-full items-center gap-2 rounded px-1.5 py-0.5 text-left font-mono text-xs"
                          title="Copy model ID"
                          onclick={() => void copyId(model.id)}
                        >
                          <Copy class="text-muted-foreground size-3 shrink-0" />
                          <span class="break-all">{model.id}</span>
                          {#if model.hint}
                            <span class="text-muted-foreground ml-auto shrink-0 text-[10px]">
                              {model.hint}
                            </span>
                          {/if}
                          {#if copied === model.id}
                            <span class="text-success shrink-0 text-[10px]">copied</span>
                          {/if}
                        </button>
                      </li>
                    {/each}
                  </ul>
                {/if}
              {/if}
            </div>
          {/if}
        </div>
      {/each}
    </div>
  {/if}
</div>

<AgentTokenDialog bind:open={tokenOpen} reason={tokenReason} onsaved={() => void load()} />
