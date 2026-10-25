<script lang="ts">
  /**
   * Cascades panel of the Config view: the selectors: section — one
   * card per selector with its strategy and ordered target chips, each
   * chip carrying the live failover health from /v1/models
   * (meta.llamaswap.failover).
   *
   * Every finite-domain field is a picker:
   *   - strategy comes from GET /agent/v1/config/schema, which serves the
   *     validator's own config.SelectorStrategies — never a UI-side copy;
   *   - a target is added by picking its type (local model | provider
   *     model) and then a model from that source's pick list, fed by this
   *     server's /v1/models and the cached provider catalogs. Targets keep
   *     their order (up/down), duplicates are skipped, and the wire format
   *     is unchanged ("existing", "peerName/modelId").
   *
   * Every write is a dry-run "Validate" then "Apply", removals ask for
   * confirmation first.
   */
  import { Button } from "$lib/components/ui/button/index.js";
  import * as Dialog from "$lib/components/ui/dialog/index.js";
  import * as Label from "$lib/components/ui/label/index.js";
  import * as Select from "$lib/components/ui/select/index.js";
  import { Input } from "$lib/components/ui/input/index.js";
  import { Plus, Pencil, Trash2, ArrowUp, ArrowDown, X, RefreshCw } from "@lucide/svelte";
  import EmptyState from "../EmptyState.svelte";
  import Tag from "../Tag.svelte";
  import ModelPicker from "./ModelPicker.svelte";
  import {
    addSelector,
    fetchPeerCatalog,
    removeSelector,
    updateSelector,
    type PeerCatalogEntry,
    type PeerState,
    type SelectorState,
  } from "../../lib/agentApi";
  import {
    catalogGroups,
    DEFAULT_SELECTOR_STRATEGY,
    describeWriteResult,
    groupsForSource,
    healthFor,
    retryLabel,
    strategyOptions,
    targetGroups,
    targetSource,
    type OutcomeBox,
    type TargetHealth,
  } from "../../lib/configUi";

  let {
    selectors = [],
    health = new Map<string, TargetHealth[]>(),
    peers = [],
    localModels = [],
    catalogs = [],
    strategies = [],
    defaultStrategy = DEFAULT_SELECTOR_STRATEGY,
    onchanged,
    onauth,
    onerror,
    onrefresh,
  }: {
    selectors: SelectorState[];
    health?: Map<string, TargetHealth[]>;
    peers?: PeerState[];
    localModels?: string[];
    /** The server's cached provider catalogs (hydration, no probing). */
    catalogs?: PeerCatalogEntry[];
    /** The strategy list from GET /agent/v1/config/schema. */
    strategies?: string[];
    defaultStrategy?: string;
    onchanged: () => void;
    onauth: (e: unknown) => boolean;
    onerror: (text: string) => void;
    /** Re-read the cached catalogs after a successful check. */
    onrefresh?: () => void;
  } = $props();

  // ---- editor dialog ------------------------------------------------------

  let editing = $state<"add" | "edit" | null>(null);
  let formName = $state("");
  let formStrategy = $state("");
  let formTargets = $state<string[]>([]);
  let formTargetInput = $state("");
  let formTargetSource = $state<"local" | "provider">("local");
  let formCooldown = $state("");
  let formSpillover = $state(1);
  let formDescription = $state("");
  let busy = $state(false);
  let outcome = $state<OutcomeBox | null>(null);
  let outcomeError = $state<string | null>(null);

  // ---- remove dialog ------------------------------------------------------

  let removing = $state<SelectorState | null>(null);
  let removingBusy = $state(false);

  function message(e: unknown): string {
    return e instanceof Error ? e.message : String(e);
  }

  let strategyList = $derived(strategyOptions(strategies));

  function selectTargetSource(value: string): void {
    if (value === "local" || value === "provider") formTargetSource = value;
  }

  function openAdd(): void {
    editing = "add";
    formName = "";
    formStrategy = strategyList.includes(defaultStrategy) ? defaultStrategy : strategyList[0];
    formTargets = [];
    formTargetInput = "";
    formTargetSource = "local";
    formCooldown = "";
    formSpillover = 1;
    formDescription = "";
    outcome = null;
    outcomeError = null;
    resetTargetPicker();
  }

  function openEdit(selector: SelectorState): void {
    editing = "edit";
    formName = selector.name;
    formStrategy = selector.strategy;
    formTargets = [...selector.targets];
    formTargetInput = "";
    formTargetSource = "local";
    formCooldown = selector.cooldown ?? "";
    formSpillover = selector.spillover > 0 ? selector.spillover : 1;
    formDescription = selector.description ?? "";
    outcome = null;
    outcomeError = null;
    resetTargetPicker();
  }

  function addTarget(): void {
    const value = formTargetInput.trim();
    if (value && !formTargets.includes(value)) {
      formTargets = [...formTargets, value];
    }
    formTargetInput = "";
  }

  // ---- target pick list (local models + per-peer catalogs) ----------------

  let targetsLoading = $state(false);
  let targetErrors = $state<string[]>([]);
  /** Catalogs fetched on demand by "Check providers now", overriding the
   * cached ones the parent handed in. */
  let liveCatalogs = $state<{ name: string; models: { id: string }[] }[] | null>(null);

  function resetTargetPicker(): void {
    targetsLoading = false;
    targetErrors = [];
    liveCatalogs = null;
  }

  let pickGroups = $derived(targetGroups(localModels, catalogGroups(liveCatalogs ?? catalogs)));
  let sourceGroups = $derived(groupsForSource(pickGroups, formTargetSource));
  let peerNames = $derived(peers.map((peer) => peer.name));

  /**
   * Probe every configured peer for its live /v1/models. Each peer is
   * queried independently, so one dead or key-rejecting provider only costs
   * its own inline error — the rest of the list still loads.
   */
  async function loadTargetOptions(): Promise<void> {
    targetsLoading = true;
    targetErrors = [];
    try {
      const results = await Promise.all(
        peers.map(async (peer) => {
          try {
            const models = await fetchPeerCatalog({ name: peer.name });
            return { name: peer.name, models: models.map((m) => ({ id: m.id })) };
          } catch (e) {
            if (onauth(e)) throw e;
            targetErrors = [...targetErrors, `peers.${peer.name}: ${message(e)}`];
            return null;
          }
        }),
      );
      liveCatalogs = results.filter(
        (r): r is { name: string; models: { id: string }[] } => r !== null,
      );
      onrefresh?.();
    } catch {
      // A 401 aborts the whole load: the token prompt is already open.
      liveCatalogs = null;
    } finally {
      targetsLoading = false;
    }
  }

  /** A picked target lands in the ordered list, duplicates skipped. */
  function addTargetFromPicker(id: string): void {
    if (!formTargets.includes(id)) {
      formTargets = [...formTargets, id];
    }
  }

  function removeTarget(index: number): void {
    formTargets = formTargets.filter((_, i) => i !== index);
  }

  function moveTarget(index: number, delta: -1 | 1): void {
    const next = index + delta;
    if (next < 0 || next >= formTargets.length) return;
    const targets = [...formTargets];
    const [moved] = targets.splice(index, 1);
    targets.splice(next, 0, moved);
    formTargets = targets;
  }

  function clientErrors(): string[] {
    const errors: string[] = [];
    if (!formName.trim()) errors.push("name is required");
    if (formTargets.length === 0) errors.push("at least one target is required");
    return errors;
  }

  function verbInput(approved: boolean) {
    const input: {
      approved: boolean;
      name: string;
      strategy?: string;
      targets?: string[];
      spillover?: number;
      cooldown?: string;
      description?: string;
    } = {
      approved,
      name: formName.trim(),
      strategy: formStrategy,
      targets: formTargets,
    };
    if (formStrategy === "failover") input.cooldown = formCooldown.trim();
    if (formStrategy === "spillover") input.spillover = formSpillover;
    if (formDescription.trim()) input.description = formDescription.trim();
    return input;
  }

  async function runVerb(approved: boolean): Promise<void> {
    outcome = null;
    outcomeError = null;
    busy = true;
    try {
      const input = verbInput(approved);
      const res = editing === "add" ? await addSelector(input) : await updateSelector(input);
      outcome = describeWriteResult(res);
      if (res.written) {
        editing = null;
        onchanged();
      }
    } catch (e) {
      if (!onauth(e)) outcomeError = message(e);
    } finally {
      busy = false;
    }
  }

  async function runRemove(): Promise<void> {
    if (!removing) return;
    removingBusy = true;
    try {
      await removeSelector(removing.name, true);
      removing = null;
      onchanged();
    } catch (e) {
      if (!onauth(e)) onerror(`could not remove selectors.${removing?.name}: ${message(e)}`);
    } finally {
      removingBusy = false;
    }
  }
</script>

<div class="space-y-3">
  <div class="flex items-center">
    <p class="text-muted-foreground text-sm">
      Cascading model chains (selectors:). Failover cascades try each target in order and skip an
      unhealthy one until its cooldown expires.
    </p>
    <div class="ml-auto pl-2">
      <Button size="sm" onclick={openAdd}>
        <Plus />
        Add cascade
      </Button>
    </div>
  </div>

  {#if selectors.length === 0}
    <EmptyState message="No selectors are defined in the config (selectors: section)." />
  {:else}
    <div class="space-y-3">
      {#each selectors as selector (selector.name)}
        <div class="space-y-2 rounded-lg border p-4">
          <div class="flex flex-wrap items-center gap-2">
            <span class="font-mono text-sm font-semibold">{selector.name}</span>
            <Tag class="bg-primary/10 text-primary">{selector.strategy}</Tag>
            {#if selector.cooldown}
              <Tag>cooldown {selector.cooldown}</Tag>
            {/if}
            {#if selector.unlisted}
              <Tag>unlisted</Tag>
            {/if}
            <div class="ml-auto flex gap-1">
              <Button variant="ghost" size="sm" onclick={() => openEdit(selector)}>
                <Pencil />
              </Button>
              <Button variant="ghost" size="sm" onclick={() => (removing = selector)}>
                <Trash2 class="text-destructive" />
              </Button>
            </div>
          </div>

          {#if selector.description}
            <p class="text-muted-foreground text-xs">{selector.description}</p>
          {/if}

          <ol class="flex flex-wrap items-center gap-1">
            {#each selector.targets as target, index (target)}
              {@const entry = healthFor(health, selector.name, target)}
              <li class="flex items-center gap-1">
                {#if index > 0}
                  <span class="text-muted-foreground text-xs">→</span>
                {/if}
                <span
                  class={`inline-flex items-center gap-1 rounded-md border px-2 py-0.5 font-mono text-xs ${entry && !entry.healthy ? "border-destructive/40 bg-destructive/10 text-destructive" : "border-success/40 bg-success/10 text-success"}`}
                  title={entry?.lastError ?? retryLabel(entry) ?? "healthy"}
                >
                  <span class={`inline-block size-1.5 rounded-full ${entry && !entry.healthy ? "bg-destructive" : "bg-success"}`}></span>
                  {target}
                </span>
              </li>
            {/each}
          </ol>
        </div>
      {/each}
    </div>
  {/if}
</div>

<Dialog.Root
  open={editing !== null}
  onOpenChange={(v) => {
    if (!v) editing = null;
  }}
>
  <Dialog.Content class="sm:max-w-lg">
    <Dialog.Header>
      <Dialog.Title>{editing === "add" ? "Add cascade" : `Edit selectors.${formName}`}</Dialog.Title>
      <Dialog.Description>
        {editing === "add"
          ? "A new selectors: stanza is appended to the config file. The change is validated first; Apply writes it and takes a backup."
          : "Only the fields below are changed. The change is validated first; Apply writes it and takes a backup."}
      </Dialog.Description>
    </Dialog.Header>

    <div class="space-y-3">
      <div class="grid grid-cols-2 gap-3">
        <div>
          <Label.Root for="selector-name" class="text-sm">Name</Label.Root>
          <Input id="selector-name" bind:value={formName} disabled={editing === "edit"} placeholder="cascade" class="mt-1" />
        </div>
        <div>
          <Label.Root class="text-sm">Strategy</Label.Root>
          <Select.Root
            type="single"
            value={formStrategy}
            onValueChange={(v) => v && (formStrategy = v)}
          >
            <Select.Trigger class="mt-1 w-full">{formStrategy}</Select.Trigger>
            <Select.Content>
              {#each strategyList as strategy (strategy)}
                <Select.Item value={strategy}>{strategy}</Select.Item>
              {/each}
            </Select.Content>
          </Select.Root>
          {#if strategies.length === 0}
            <p class="text-muted-foreground mt-1 text-[10px]">
              Server strategy list unavailable — showing the built-in set.
            </p>
          {/if}
        </div>
      </div>

      <div>
        <Label.Root class="text-sm">Targets (in order)</Label.Root>
        {#if formTargets.length > 0}
          <ol class="mt-1 space-y-1">
            {#each formTargets as target, index (target)}
              <li class="flex items-center gap-1 rounded-md border px-2 py-1">
                <span class="text-muted-foreground w-5 text-right text-xs">{index + 1}.</span>
                <span class="text-muted-foreground rounded border px-1 text-[10px] uppercase">
                  {targetSource(target, localModels, peerNames) === "local" ? "local" : "peer"}
                </span>
                <span class="flex-1 font-mono text-xs">{target}</span>
                <Button variant="ghost" size="sm" disabled={index === 0} onclick={() => moveTarget(index, -1)}>
                  <ArrowUp />
                </Button>
                <Button variant="ghost" size="sm" disabled={index === formTargets.length - 1} onclick={() => moveTarget(index, 1)}>
                  <ArrowDown />
                </Button>
                <Button variant="ghost" size="sm" onclick={() => removeTarget(index)}>
                  <X class="text-destructive" />
                </Button>
              </li>
            {/each}
          </ol>
        {/if}

        <div class="mt-2 rounded-md border p-2">
          <div class="flex flex-wrap items-center gap-2">
            <span class="text-xs font-medium">Add a target</span>
            <Select.Root
              type="single"
              value={formTargetSource}
              onValueChange={(v) => v && selectTargetSource(v)}
            >
              <Select.Trigger class="w-40 text-xs">
                {formTargetSource === "local" ? "local model" : "provider model"}
              </Select.Trigger>
              <Select.Content>
                <Select.Item value="local">local model</Select.Item>
                <Select.Item value="provider">provider model</Select.Item>
              </Select.Content>
            </Select.Root>
            <Button
              variant="outline"
              size="sm"
              disabled={targetsLoading || peers.length === 0}
              onclick={() => void loadTargetOptions()}
            >
              <RefreshCw class={targetsLoading ? "animate-spin" : ""} />
              {targetsLoading ? "Checking…" : "Check providers now"}
            </Button>
          </div>
          <p class="text-muted-foreground mt-1 text-xs">
            Local models come from this server's /v1/models; provider models from the cached
            provider catalogs (peerName/modelId). Each provider is checked on its own; a provider
            that fails only costs its entry.
          </p>
          {#if targetErrors.length > 0}
            <ul class="mt-2 space-y-0.5">
              {#each targetErrors as err (err)}
                <li class="text-destructive text-xs">{err}</li>
              {/each}
            </ul>
          {/if}
          <div class="mt-2">
            <ModelPicker
              groups={sourceGroups}
              loading={targetsLoading}
              emptyText={formTargetSource === "local"
                ? "No local models listed yet."
                : "No cached provider catalog yet; press Check providers now, or type a target."}
              placeholder={formTargetSource === "local"
                ? "Filter local models…"
                : "Filter provider models…"}
              onpick={addTargetFromPicker}
            />
          </div>
          <div class="mt-2 flex gap-2">
            <Input
              id="selector-target-input"
              bind:value={formTargetInput}
              placeholder="existing, peerName/modelId"
              class="text-xs"
              onkeydown={(e) => {
                if (e.key === "Enter") {
                  e.preventDefault();
                  addTarget();
                }
              }}
            />
            <Button variant="outline" size="sm" onclick={addTarget}>
              <Plus />
              Add
            </Button>
          </div>
        </div>
      </div>

      {#if formStrategy === "failover"}
        <div>
          <Label.Root for="selector-cooldown" class="text-sm">Cooldown</Label.Root>
          <Input id="selector-cooldown" bind:value={formCooldown} placeholder="90s" class="mt-1" />
          <p class="text-muted-foreground mt-1 text-xs">
            How long an unhealthy target is skipped before it is retried (a Go duration like 90s or
            1m30s; empty uses the default).
          </p>
        </div>
      {:else if formStrategy === "spillover"}
        <div>
          <Label.Root for="selector-spillover" class="text-sm">Spillover limit</Label.Root>
          <Input id="selector-spillover" type="number" min="1" bind:value={formSpillover} class="mt-1" />
        </div>
      {/if}

      <div>
        <Label.Root for="selector-description" class="text-sm">Description (optional)</Label.Root>
        <Input id="selector-description" bind:value={formDescription} class="mt-1" />
      </div>
    </div>

    {#if outcome}
      <div class={`mt-3 rounded-md border p-3 text-xs ${outcome.ok ? "border-success/40 bg-success/10" : "border-destructive/40 bg-destructive/10"}`}>
        <div class={outcome.ok ? "text-success font-medium" : "text-destructive font-medium"}>{outcome.text}</div>
        {#if outcome.detail.length}
          <ul class="text-muted-foreground mt-1 list-disc space-y-0.5 pl-4">
            {#each outcome.detail as line (line)}
              <li>{line}</li>
            {/each}
          </ul>
        {/if}
      </div>
    {/if}
    {#if outcomeError}
      <div class="text-destructive mt-3 rounded-md border border-destructive/40 bg-destructive/10 p-3 text-xs font-medium">
        {outcomeError}
      </div>
    {/if}

    <Dialog.Footer>
      <Button variant="outline" onclick={() => (editing = null)}>Cancel</Button>
      <Button
        variant="outline"
        disabled={busy || clientErrors().length > 0}
        onclick={() => void runVerb(false)}
      >
        Validate
      </Button>
      <Button
        disabled={busy || clientErrors().length > 0}
        onclick={() => void runVerb(true)}
      >
        {editing === "add" ? "Add" : "Apply"}
      </Button>
    </Dialog.Footer>
  </Dialog.Content>
</Dialog.Root>

<Dialog.Root
  open={removing !== null}
  onOpenChange={(v) => {
    if (!v) removing = null;
  }}
>
  <Dialog.Content class="sm:max-w-md">
    <Dialog.Header>
      <Dialog.Title>Remove selectors.{removing?.name ?? ""}?</Dialog.Title>
      <Dialog.Description>
        The cascade is deleted from the config file (a timestamped backup is taken first). Clients
        routing to it get an unknown-model error until it is recreated.
      </Dialog.Description>
    </Dialog.Header>
    <Dialog.Footer>
      <Button variant="outline" onclick={() => (removing = null)}>Cancel</Button>
      <Button variant="destructive" disabled={removingBusy} onclick={() => void runRemove()}>
        Remove
      </Button>
    </Dialog.Footer>
  </Dialog.Content>
</Dialog.Root>
