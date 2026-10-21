<script lang="ts">
  /**
   * Engines view: the configured engines section and, per engine with
   * an update source, the engine updater. All data comes from the
   * bearer-authenticated /agent/v1 API:
   *
   *   GET  /agent/v1/engines                     list (cmd path, env names)
   *   GET  /agent/v1/engines/updates             installed version / check state
   *   POST /agent/v1/engines/check              "Check for updates" (sources)
   *   POST /agent/v1/engines/{name}/update      swap, only when approved
   *   POST /agent/v1/engines/{name}/rollback    restore previous, only when approved
   */
  import { onMount } from "svelte";
  import { Button } from "$lib/components/ui/button/index.js";
  import * as Dialog from "$lib/components/ui/dialog/index.js";
  import * as Switch from "$lib/components/ui/switch/index.js";
  import * as Label from "$lib/components/ui/label/index.js";
  import { RefreshCw, RotateCcw, ShieldAlert, Download, SearchCheck } from "@lucide/svelte";
  import AgentTokenDialog from "../components/AgentTokenDialog.svelte";
  import EmptyState from "../components/EmptyState.svelte";
  import Tag from "../components/Tag.svelte";
  import {
    AgentApiError,
    AgentUnauthorizedError,
    checkEngineUpdates,
    listEngineUpdates,
    listEngines,
    rollbackEngine,
    updateEngine,
    currentToken,
    type EngineCheckResult,
    type EngineInfo,
    type EngineUpdateInfo,
  } from "../lib/agentApi";
  import {
    formatTimestamp,
    refusalMessage,
    summarizeCheckResult,
    summarizeRollbackResult,
    summarizeUpdateResult,
    type EngineOutcome,
  } from "../lib/engineUpdates";

  let engines = $state<EngineInfo[] | null>(null);
  let updates = $state<Map<string, EngineUpdateInfo>>(new Map());
  let loading = $state(false);
  let error = $state<string | null>(null);

  // Which engine a slow button belongs to; "" = the check-all button.
  let busy = $state<string | null>(null);

  // Last operation result per engine, rendered under the buttons.
  let results = $state<Record<string, EngineOutcome>>({});

  // The confirmation dialog for update/rollback. kind is null when closed.
  let confirmKind = $state<"update" | "rollback" | null>(null);
  let confirmEngine = $state<EngineInfo | null>(null);
  let confirmForce = $state(false);

  let tokenOpen = $state(false);
  let tokenReason = $state("");

  function message(e: unknown): string {
    return e instanceof Error ? e.message : String(e);
  }

  function openTokenPrompt(reason: string): void {
    tokenReason = reason;
    tokenOpen = true;
  }

  /** Load both lists. Prompts for the token on first use and on 401s. */
  async function load(): Promise<void> {
    if (!currentToken()) {
      openTokenPrompt("");
      return;
    }
    loading = true;
    error = null;
    try {
      const [engineList, updateList] = await Promise.all([listEngines(), listEngineUpdates()]);
      engines = engineList;
      updates = new Map(updateList.map((u) => [u.name, u]));
    } catch (e) {
      if (e instanceof AgentUnauthorizedError) {
        openTokenPrompt("The saved key was rejected (401). Enter the agent_api key of this server.");
      } else {
        error = message(e);
      }
    } finally {
      loading = false;
    }
  }

  onMount(load);

  function updateFor(name: string): EngineUpdateInfo | undefined {
    return updates.get(name);
  }

  function availableTag(name: string): string | undefined {
    return updateFor(name)?.updateAvailable?.tag;
  }

  function fmtTime(iso?: string): string {
    return formatTimestamp(iso);
  }

  /** One engine's buttons stay disabled while its request is in flight. */
  function isBusy(key: string): boolean {
    return busy === key;
  }

  function setResult(name: string, outcome: EngineOutcome): void {
    results[name] = outcome;
  }

  function authProblem(e: unknown): boolean {
    if (e instanceof AgentUnauthorizedError) {
      openTokenPrompt("The saved key was rejected (401). Enter the agent_api key of this server.");
      return true;
    }
    return false;
  }

  /** "Check for updates" for one engine (or all when name is ""). */
  async function doCheck(name: string): Promise<void> {
    busy = name;
    try {
      const checked: EngineCheckResult[] =
        name === "" ? await checkEngineUpdates() : await checkEngineUpdates([name]);
      for (const row of checked) {
        setResult(row.engine, summarizeCheckResult(row));
      }
      // Refresh the recorded state (lastCheckAt, updateAvailable).
      updates = new Map((await listEngineUpdates()).map((u) => [u.name, u]));
    } catch (e) {
      if (!authProblem(e)) setResult(name, { ok: false, text: `check failed: ${message(e)}`, detail: [] });
    } finally {
      busy = null;
    }
  }

  function openConfirm(kind: "update" | "rollback", engine: EngineInfo): void {
    confirmKind = kind;
    confirmEngine = engine;
    confirmForce = false;
  }

  async function doUpdate(engine: EngineInfo, force: boolean): Promise<void> {
    busy = engine.name;
    try {
      setResult(engine.name, summarizeUpdateResult(await updateEngine(engine.name, { approved: true, force })));
      updates = new Map((await listEngineUpdates()).map((u) => [u.name, u]));
    } catch (e) {
      if (!authProblem(e)) {
        const conflict = e instanceof AgentApiError && e.status === 409;
        setResult(
          engine.name,
          conflict
            ? { ok: false, text: refusalMessage(message(e)), detail: [] }
            : { ok: false, text: `update failed: ${message(e)}`, detail: [] },
        );
      }
    } finally {
      busy = null;
    }
  }

  async function doRollback(engine: EngineInfo, force: boolean): Promise<void> {
    busy = engine.name;
    try {
      setResult(engine.name, summarizeRollbackResult(await rollbackEngine(engine.name, { approved: true, force })));
      updates = new Map((await listEngineUpdates()).map((u) => [u.name, u]));
    } catch (e) {
      if (!authProblem(e)) {
        const conflict = e instanceof AgentApiError && e.status === 409;
        setResult(
          engine.name,
          conflict
            ? { ok: false, text: refusalMessage(message(e)), detail: [] }
            : { ok: false, text: `rollback failed: ${message(e)}`, detail: [] },
        );
      }
    } finally {
      busy = null;
    }
  }

  /** The dialog's confirm button dispatches to the pending operation. */
  function handleConfirm(): void {
    const engine = confirmEngine;
    const kind = confirmKind;
    const force = confirmForce;
    confirmKind = null;
    if (!engine || !kind) return;
    if (kind === "update") void doUpdate(engine, force);
    else void doRollback(engine, force);
  }
</script>

<div class="mx-auto max-w-3xl space-y-4 p-2">
  <div class="flex flex-wrap items-center gap-2">
    <h3 class="text-lg font-semibold">Engines</h3>
    <div class="ml-auto flex flex-wrap gap-2">
      <Button variant="outline" size="sm" onclick={() => doCheck("")} disabled={loading || isBusy("")}>
        <SearchCheck />
        Check all
      </Button>
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

  {#if loading && !engines}
    <EmptyState message="Loading engines…" />
  {:else if engines && engines.length === 0}
    <EmptyState message="No engines are defined in the config (engines: section)." />
  {:else if engines}
    <div class="space-y-3">
      {#each engines as engine (engine.name)}
        {@const update = updateFor(engine.name)}
        <div class="space-y-3 rounded-lg border p-4">
          <div class="flex flex-wrap items-center gap-2">
            <span class="font-mono text-sm font-semibold">{engine.name}</span>
            {#if engine.label}
              <span class="text-muted-foreground text-sm">{engine.label}</span>
            {/if}
            {#if update?.updateAvailable}
              <Tag class="bg-success/15 text-success">{update.updateAvailable.tag} available</Tag>
            {/if}
            {#if !update?.hasUpdateSource}
              <Tag>no update source</Tag>
            {/if}
          </div>

          <div class="text-muted-foreground space-y-1 font-mono text-xs break-all">
            <div class="truncate" title={engine.cmd}>{engine.cmd}</div>
            {#if engine.envKeys.length > 0}
              <div class="flex flex-wrap gap-1">
                <span class="text-muted-foreground/70 self-center font-sans">env:</span>
                {#each engine.envKeys as key (key)}
                  <Tag>{key}</Tag>
                {/each}
              </div>
            {/if}
          </div>

          {#if update}
            <dl class="text-muted-foreground grid grid-cols-2 gap-x-4 gap-y-1 text-xs sm:grid-cols-4">
              <div>
                <dt class="font-sans">installed</dt>
                <dd class="font-medium text-foreground">{update.installedVersion ?? "unknown"}</dd>
              </div>
              <div>
                <dt class="font-sans">source</dt>
                <dd class="font-medium text-foreground">{update.source ?? "—"}</dd>
              </div>
              <div>
                <dt class="font-sans">last check</dt>
                <dd class="font-medium text-foreground">{fmtTime(update.lastCheckAt)}</dd>
              </div>
              <div>
                <dt class="font-sans">last update</dt>
                <dd class="font-medium text-foreground">{fmtTime(update.lastUpdateAt)}</dd>
              </div>
            </dl>
          {/if}

          {#if update?.hasUpdateSource}
            <div class="flex flex-wrap gap-2">
              <Button
                variant="outline"
                size="sm"
                onclick={() => doCheck(engine.name)}
                disabled={isBusy(engine.name)}
              >
                <RefreshCw class={isBusy(engine.name) ? "animate-spin" : ""} />
                Check for updates
              </Button>
              <Button variant="outline" size="sm" onclick={() => openConfirm("update", engine)} disabled={isBusy(engine.name)}>
                <Download />
                Update
              </Button>
              <Button variant="outline" size="sm" onclick={() => openConfirm("rollback", engine)} disabled={isBusy(engine.name)}>
                <RotateCcw />
                Rollback
              </Button>
            </div>
          {/if}

          {#if results[engine.name]}
            <div
              class={`rounded-md border p-3 text-xs ${results[engine.name].ok
                ? "border-success/40 bg-success/10"
                : "border-destructive/40 bg-destructive/10"}`}
            >
              <div class={results[engine.name].ok ? "text-success font-medium" : "text-destructive font-medium"}>
                {results[engine.name].text}
              </div>
              {#if results[engine.name].detail?.length}
                <ul class="text-muted-foreground mt-1 list-disc space-y-0.5 pl-4">
                  {#each results[engine.name].detail ?? [] as line (line)}
                    <li>{line}</li>
                  {/each}
                </ul>
              {/if}
            </div>
          {/if}
        </div>
      {/each}
    </div>
  {/if}
</div>

<Dialog.Root
  open={confirmKind !== null}
  onOpenChange={(v) => {
    if (!v) confirmKind = null;
  }}
>
  <Dialog.Content class="sm:max-w-md">
    <Dialog.Header>
      <Dialog.Title class="flex items-center gap-2">
        <ShieldAlert class="text-destructive size-5" />
        {confirmKind === "update" ? "Update engine" : "Roll back engine"}
        <span class="text-muted-foreground font-mono text-sm font-normal">{confirmEngine?.name}</span>
      </Dialog.Title>
      <Dialog.Description>
        {#if confirmKind === "update"}
          The newest matched release
          {#if availableTag(confirmEngine?.name ?? "")}
            ({availableTag(confirmEngine?.name ?? "")})
          {/if}
          is downloaded, checksum-verified and swapped in place of the current binary. Models using
          the engine are stopped first when "stop models" is checked, and stay stopped. The current
          binary is kept for rollback.
        {:else}
          The previous binary (kept by the last update) is restored in its place. Models using the
          engine are stopped first when "stop models" is checked, and stay stopped. The swapped-out
          build becomes the new rollback slot, so this is reversible.
        {/if}
      </Dialog.Description>
    </Dialog.Header>

    <div class="flex items-start justify-between gap-4 py-2">
      <div>
        <Label.Root for="confirm-force" class="text-sm">Stop running models first</Label.Root>
        <p class="text-muted-foreground text-xs">
          Without this, the swap is refused while any model on this engine is running.
        </p>
      </div>
      <Switch.Root id="confirm-force" bind:checked={confirmForce} />
    </div>

    <Dialog.Footer>
      <Button variant="outline" onclick={() => (confirmKind = null)}>Cancel</Button>
      <Button variant="destructive" onclick={handleConfirm}>
        {confirmKind === "update" ? "Update" : "Roll back"}
      </Button>
    </Dialog.Footer>
  </Dialog.Content>
</Dialog.Root>

<AgentTokenDialog
  bind:open={tokenOpen}
  reason={tokenReason}
  onsaved={() => void load()}
/>
