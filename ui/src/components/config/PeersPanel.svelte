<script lang="ts">
  /**
   * Peers panel of the Config view: the peers: section as a table with
   * add/edit/remove forms.
   *
   * Every finite-domain field is a picker:
   *   - name is free text (it is the identifier);
   *   - proxy is a select of the proxies already configured plus the
   *     server's presets, with "custom…" revealing the free-text input and
   *     the same validation as before;
   *   - apiKey is a select of ${env.VAR} NAMES the server knows (flagged
   *     when they no longer resolve) with "other macro…" and a "literal
   *     (discouraged)" fallback — literals stay rejected, exactly as today;
   *   - models is a chip list fed by the server's cached provider catalog,
   *     with hand-typed IDs kept as "unverified" chips because models come
   *     and go.
   *
   * Reads and writes never carry a key value: the browser only ever sees
   * the macro NAME. Every write is a dry-run "Validate" (approved: false)
   * followed by "Apply"; removals ask for confirmation first.
   */
  import { Button } from "$lib/components/ui/button/index.js";
  import * as Dialog from "$lib/components/ui/dialog/index.js";
  import * as Label from "$lib/components/ui/label/index.js";
  import * as Select from "$lib/components/ui/select/index.js";
  import { Input } from "$lib/components/ui/input/index.js";
  import { Plus, Pencil, Trash2, X, RefreshCw, CircleAlert, CircleCheck } from "@lucide/svelte";
  import EmptyState from "../EmptyState.svelte";
  import Tag from "../Tag.svelte";
  import ModelPicker from "./ModelPicker.svelte";
  import {
    addPeer,
    probePeerCatalog,
    removePeer,
    updatePeer,
    type EnvName,
    type PeerCatalogEntry,
    type PeerCatalogModel,
    type PeerState,
    type ProxyPreset,
  } from "../../lib/agentApi";
  import {
    appendModelToText,
    catalogIds,
    describeWriteResult,
    envMacroError,
    KEY_CUSTOM,
    KEY_LITERAL,
    KEY_NONE,
    keyChoices,
    keyChoiceFor,
    modelsFromText,
    modelsToText,
    pickHint,
    PROXY_CUSTOM,
    proxyChoices,
    secretLabel,
    type ModelPickOption,
    type OutcomeBox,
  } from "../../lib/configUi";

  let {
    peers = [],
    catalogs = [],
    envNames = [],
    proxyPresets = [],
    onchanged,
    onauth,
    onerror,
    onrefresh,
  }: {
    peers: PeerState[];
    /** The server's cached provider catalogs (GET /agent/v1/config/peers.catalog). */
    catalogs?: PeerCatalogEntry[];
    /** ${env.VAR} NAMES the server environment offers (never values). */
    envNames?: EnvName[];
    /** Proxy presets from GET /agent/v1/config/schema. */
    proxyPresets?: ProxyPreset[];
    onchanged: () => void;
    onauth: (e: unknown) => boolean;
    onerror: (text: string) => void;
    /** Re-read the cached catalogs after a successful check. */
    onrefresh?: () => void;
  } = $props();

  // ---- editor dialog ------------------------------------------------------

  let editing = $state<"add" | "edit" | null>(null);
  let editingPeer = $state<PeerState | null>(null);
  let formName = $state("");
  let formProxy = $state("");
  let formApiKey = $state("");
  let formModels = $state("");
  let formKeyTouched = $state(false);
  let formModelInput = $state("");
  let proxySelection = $state("");
  let keySelection = $state<string>(KEY_NONE);
  let busy = $state(false);
  let outcome = $state<OutcomeBox | null>(null);
  let outcomeError = $state<string | null>(null);

  // ---- provider catalog (the models pick list) ----------------------------

  let liveCatalog = $state<PeerCatalogModel[] | null>(null);
  let catalogLoading = $state(false);
  let catalogError = $state<string>("");

  // ---- remove dialog ------------------------------------------------------

  let removing = $state<PeerState | null>(null);
  let removingBusy = $state(false);

  function message(e: unknown): string {
    return e instanceof Error ? e.message : String(e);
  }

  // The proxies to offer: the config's own peers plus the server's presets.
  let proxyOptions = $derived(proxyChoices(peers, proxyPresets));

  function openAdd(): void {
    editing = "add";
    editingPeer = null;
    formName = "";
    formProxy = "";
    formApiKey = "";
    formModels = "";
    formModelInput = "";
    formKeyTouched = false;
    proxySelection = PROXY_CUSTOM;
    keySelection = KEY_NONE;
    outcome = null;
    outcomeError = null;
    resetCatalog();
  }

  function openEdit(peer: PeerState): void {
    editing = "edit";
    editingPeer = peer;
    formName = peer.name;
    formProxy = peer.proxy;
    formApiKey = peer.apiKey.macro ?? "";
    formModels = modelsToText(peer.models);
    formModelInput = "";
    formKeyTouched = peer.apiKey.set;
    proxySelection = proxyOptions.some((choice) => choice.value === peer.proxy)
      ? peer.proxy
      : PROXY_CUSTOM;
    keySelection = keyChoiceFor(
      peer.apiKey.macro ?? "",
      keyChoices(envNames, peer.apiKey.macro ?? ""),
    );
    outcome = null;
    outcomeError = null;
    resetCatalog();
  }

  function resetCatalog(): void {
    liveCatalog = null;
    catalogLoading = false;
    catalogError = "";
  }

  // ---- models chips -------------------------------------------------------

  /** The peer's cached catalog: the server's record, or a fresh live probe. */
  let peerCatalog = $derived.by(() => {
    if (liveCatalog !== null) return liveCatalog;
    return catalogs.find((entry) => entry.name === formName)?.models ?? [];
  });
  let verifiedIds = $derived(catalogIds(peerCatalog));
  let modelChips = $derived(
    modelsFromText(formModels).map((id) => ({ id, verified: verifiedIds.includes(id) })),
  );
  let pickGroups = $derived(
    peerCatalog.length > 0
      ? [
          {
            label: `peers.${formName.trim() || "provider"}`,
            options: peerCatalog.map(
              (model): ModelPickOption => ({
                id: model.id,
                hint: pickHint(model.ownedBy, model.contextLength),
              }),
            ),
          },
        ]
      : [],
  );

  function addModel(id: string): void {
    formModels = appendModelToText(formModels, id);
  }

  function addTypedModel(): void {
    const id = formModelInput.trim();
    if (!id) return;
    addModel(id);
    formModelInput = "";
  }

  function removeModel(id: string): void {
    formModels = modelsFromText(formModels)
      .filter((model) => model !== id)
      .join(", ");
  }

  /**
   * Query the provider live for its /v1/models and show the result in the
   * picker. A failure is inline and never blocks the form. On success the
   * server also refreshes its catalog cache, so the parent re-reads it.
   */
  async function checkModels(): Promise<void> {
    catalogLoading = true;
    catalogError = "";
    try {
      const samePeer =
        editing === "edit" &&
        editingPeer !== null &&
        formProxy.trim() === editingPeer.proxy &&
        formApiKey.trim() === (editingPeer.apiKey.macro ?? "");
      const key = formApiKey.trim();
      const input = samePeer
        ? { name: formName.trim() }
        : { proxy: formProxy.trim(), ...(key ? { apiKey: key } : {}) };
      const probe = await probePeerCatalog(input);
      liveCatalog = probe.models;
      onrefresh?.();
    } catch (e) {
      liveCatalog = null;
      if (!onauth(e)) catalogError = message(e);
    } finally {
      catalogLoading = false;
    }
  }

  // ---- key select ---------------------------------------------------------

  let keyOptions = $derived(keyChoices(envNames, editingPeer?.apiKey.macro ?? ""));

  function selectKey(value: string): void {
    keySelection = value;
    if (value === KEY_CUSTOM || value === KEY_LITERAL) {
      // Both reveal the free-text field; only a macro passes validation.
      formApiKey = "";
      formKeyTouched = true;
      return;
    }
    formApiKey = value === KEY_NONE ? "" : value;
    formKeyTouched = true;
  }

  /** The macro rule, checked in the browser before anything is sent. */
  function keyError(): string | null {
    return envMacroError("apiKey", formApiKey);
  }

  function clientErrors(): string[] {
    const errors: string[] = [];
    if (!formName.trim()) errors.push("name is required");
    if (!formProxy.trim()) errors.push("proxy is required");
    if (modelsFromText(formModels).length === 0) errors.push("at least one model is required");
    const key = keyError();
    if (key) errors.push(key);
    return errors;
  }

  /** Build the verb input; "" keeps the configured value for update. */
  function verbInput(approved: boolean) {
    if (editing === "add") {
      return {
        approved,
        name: formName.trim(),
        proxy: formProxy.trim(),
        apiKey: formApiKey.trim(),
        models: modelsFromText(formModels),
      };
    }
    return {
      approved,
      name: formName.trim(),
      proxy: formProxy.trim(),
      // Only send the key when it was edited; "" alone would clear it.
      apiKey: formKeyTouched ? formApiKey.trim() : "",
      models: modelsFromText(formModels),
    };
  }

  async function runVerb(approved: boolean): Promise<void> {
    outcome = null;
    outcomeError = null;
    busy = true;
    try {
      const input = verbInput(approved);
      const res = editing === "add" ? await addPeer(input) : await updatePeer(input);
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
      await removePeer(removing.name, true);
      removing = null;
      onchanged();
    } catch (e) {
      if (!onauth(e)) onerror(`could not remove peers.${removing?.name}: ${message(e)}`);
    } finally {
      removingBusy = false;
    }
  }
</script>

<div class="space-y-3">
  <div class="flex items-center">
    <p class="text-muted-foreground text-sm">
      Upstream llama-swap instances this server routes to. API keys are referenced by environment
      variable, never stored in the browser.
    </p>
    <div class="ml-auto pl-2">
      <Button size="sm" onclick={openAdd}>
        <Plus />
        Add peer
      </Button>
    </div>
  </div>

  {#if peers.length === 0}
    <EmptyState message="No peers are defined in the config (peers: section)." />
  {:else}
    <div class="overflow-x-auto rounded-lg border">
      <table class="w-full text-sm">
        <thead>
          <tr class="text-muted-foreground border-b text-left text-xs uppercase">
            <th class="px-3 py-2">Name</th>
            <th class="px-3 py-2">Proxy</th>
            <th class="px-3 py-2">API key</th>
            <th class="px-3 py-2">Models</th>
            <th class="px-3 py-2"></th>
          </tr>
        </thead>
        <tbody>
          {#each peers as peer (peer.name)}
            <tr class="border-b last:border-b-0">
              <td class="px-3 py-2 font-mono">{peer.name}</td>
              <td class="px-3 py-2 font-mono text-xs break-all">{peer.proxy}</td>
              <td class="px-3 py-2">
                <span class="font-mono text-xs">{secretLabel(peer.apiKey)}</span>
                {#if peer.apiKey.macro && catalogs.find((c) => c.name === peer.name)?.keyMacroAvailable === false}
                  <span class="text-destructive block text-[10px]">macro not resolvable</span>
                {/if}
              </td>
              <td class="px-3 py-2">
                <div class="flex max-w-xs flex-wrap gap-1">
                  {#each peer.models as model (model)}
                    <Tag>{model}</Tag>
                  {/each}
                </div>
              </td>
              <td class="px-3 py-2 text-right">
                <div class="inline-flex gap-1">
                  <Button variant="ghost" size="sm" onclick={() => openEdit(peer)}>
                    <Pencil />
                  </Button>
                  <Button variant="ghost" size="sm" onclick={() => (removing = peer)}>
                    <Trash2 class="text-destructive" />
                  </Button>
                </div>
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
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
      <Dialog.Title>{editing === "add" ? "Add peer" : `Edit peers.${formName}`}</Dialog.Title>
      <Dialog.Description>
        {editing === "add"
          ? "A new peers: stanza is appended to the config file. The change is validated first; Apply writes it and takes a backup."
          : "Only the fields below are changed; everything else in the stanza is preserved. The change is validated first; Apply writes it and takes a backup."}
      </Dialog.Description>
    </Dialog.Header>

    <div class="space-y-3">
      <div>
        <Label.Root for="peer-name" class="text-sm">Name</Label.Root>
        <Input id="peer-name" bind:value={formName} disabled={editing === "edit"} placeholder="openrouter" class="mt-1" />
      </div>

      <div>
        <Label.Root class="text-sm">Proxy URL</Label.Root>
        <Select.Root
          type="single"
          value={proxySelection}
          onValueChange={(v) => {
            if (!v) return;
            proxySelection = v;
            formProxy = v === PROXY_CUSTOM ? "" : v;
          }}
        >
          <Select.Trigger class="mt-1 w-full font-mono text-xs">
            {proxySelection === PROXY_CUSTOM ? "custom…" : proxySelection}
          </Select.Trigger>
          <Select.Content>
            {#each proxyOptions as choice (choice.value)}
              <Select.Item value={choice.value}>{choice.label}</Select.Item>
            {/each}
            <Select.Item value={PROXY_CUSTOM}>custom…</Select.Item>
          </Select.Content>
        </Select.Root>
        {#if proxySelection === PROXY_CUSTOM}
          <Input
            id="peer-proxy"
            bind:value={formProxy}
            placeholder="https://openrouter.ai/api"
            class="mt-1 font-mono text-xs"
            aria-label="Custom proxy URL"
          />
        {/if}
        {#if !formProxy.trim()}
          <p class="text-destructive mt-1 text-xs">proxy is required</p>
        {/if}
      </div>

      <div>
        <Label.Root class="text-sm">API key (env macro name only)</Label.Root>
        <Select.Root type="single" value={keySelection} onValueChange={(v) => v && selectKey(v)}>
          <Select.Trigger class="mt-1 w-full font-mono text-xs">
            {keyOptions.find((choice) => choice.value === keySelection)?.label ?? "No key"}
          </Select.Trigger>
          <Select.Content>
            {#each keyOptions as choice (choice.value)}
              <Select.Item value={choice.value}>{choice.label}</Select.Item>
            {/each}
          </Select.Content>
        </Select.Root>
        {#if keySelection === KEY_CUSTOM || keySelection === KEY_LITERAL}
          <Input
            id="peer-api-key"
            bind:value={formApiKey}
            oninput={() => (formKeyTouched = true)}
            placeholder={'${env.MY_PEER_KEY}'}
            class="mt-1 font-mono"
            aria-label="Key environment macro"
          />
        {/if}
        <p class="text-muted-foreground mt-1 text-xs">
          {#if keyError()}
            <span class="text-destructive">{keyError()}</span>
          {:else}
            An {'${env.VAR}'} reference the server resolves at startup. Literal secrets are
            rejected; the browser never holds a key value.
          {/if}
        </p>
      </div>

      <div>
        <Label.Root class="text-sm">Models</Label.Root>
        {#if modelChips.length > 0}
          <div class="mt-1 flex flex-wrap gap-1">
            {#each modelChips as chip (chip.id)}
              <span
                class={`inline-flex items-center gap-1 rounded-md border px-2 py-0.5 font-mono text-xs ${chip.verified ? "border-success/40 bg-success/10" : "border-warning/40 bg-warning/10"}`}
                title={chip.verified
                  ? "in the provider's last catalog"
                  : "not in the provider's last catalog — entered by hand"}
              >
                {chip.id}
                {#if chip.verified}
                  <CircleCheck class="size-3" />
                {:else}
                  <CircleAlert class="text-warning size-3" />
                  <span class="sr-only">unverified</span>
                {/if}
                <button
                  type="button"
                  class="hover:text-destructive"
                  aria-label={`Remove ${chip.id}`}
                  onclick={() => removeModel(chip.id)}
                >
                  <X class="size-3" />
                </button>
              </span>
            {/each}
          </div>
          {#if modelChips.some((chip) => !chip.verified)}
            <p class="text-muted-foreground mt-1 text-[10px]">
              Unverified: hand-entered IDs the provider did not list in the last check.
            </p>
          {/if}
        {:else}
          <p class="text-muted-foreground mt-1 text-xs">No models yet.</p>
        {/if}

        <div class="mt-2 flex gap-2">
          <Input
            bind:value={formModelInput}
            placeholder="model-id (added as unverified)"
            class="font-mono text-xs"
            aria-label="Model ID to add"
            onkeydown={(e) => {
              if (e.key === "Enter") {
                e.preventDefault();
                addTypedModel();
              }
            }}
          />
          <Button variant="outline" size="sm" onclick={addTypedModel}>
            <Plus />
            Add
          </Button>
        </div>

        <div class="mt-2 rounded-md border p-2">
          <div class="flex flex-wrap items-center gap-2">
            <span class="text-xs font-medium">Provider catalog</span>
            <Button
              variant="outline"
              size="sm"
              disabled={catalogLoading || !formProxy.trim()}
              onclick={() => void checkModels()}
            >
              <RefreshCw class={catalogLoading ? "animate-spin" : ""} />
              {catalogLoading ? "Checking…" : "Check models now"}
            </Button>
            {#if verifiedIds.length > 0}
              <span class="text-muted-foreground text-[10px]">{verifiedIds.length} known</span>
            {/if}
          </div>
          <p class="text-muted-foreground mt-1 text-xs">
            The last catalog this server cached, or a fresh check over the provider's own
            /v1/models. The key never leaves the server.
          </p>
          <div class="mt-2">
            <ModelPicker
              groups={pickGroups}
              loading={catalogLoading}
              error={catalogError}
              emptyText={formProxy.trim()
                ? "No catalog cached for this provider yet; press Check models now, or add IDs by hand."
                : "Enter a proxy first."}
              placeholder="Filter provider models…"
              onpick={addModel}
            />
          </div>
        </div>
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
      <Dialog.Title>Remove peers.{removing?.name ?? ""}?</Dialog.Title>
      <Dialog.Description>
        The stanza is deleted from the config file (a timestamped backup is taken first). A peer
        still referenced by a selector is rejected by the validator.
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
