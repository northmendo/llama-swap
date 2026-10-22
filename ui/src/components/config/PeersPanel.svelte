<script lang="ts">
  /**
   * Peers panel of the Config view: the peers: section as a table with
   * add/edit/remove forms. Key fields accept an ${env.VAR} macro name
   * ONLY — the client rejects a literal secret before anything is sent
   * and the server rejects it again on the other end. Every write is a
   * dry-run "Validate" (approved: false) followed by "Apply"; removals
   * ask for confirmation first.
   */
  import { Button } from "$lib/components/ui/button/index.js";
  import * as Dialog from "$lib/components/ui/dialog/index.js";
  import * as Label from "$lib/components/ui/label/index.js";
  import { Input } from "$lib/components/ui/input/index.js";
  import { Textarea } from "$lib/components/ui/textarea/index.js";
  import { Plus, Pencil, Trash2 } from "@lucide/svelte";
  import EmptyState from "../EmptyState.svelte";
  import Tag from "../Tag.svelte";
  import {
    addPeer,
    removePeer,
    updatePeer,
    type PeerState,
  } from "../../lib/agentApi";
  import {
    describeWriteResult,
    envMacroError,
    modelsFromText,
    modelsToText,
    secretLabel,
    type OutcomeBox,
  } from "../../lib/configUi";

  let {
    peers = [],
    onchanged,
    onauth,
    onerror,
  }: {
    peers: PeerState[];
    onchanged: () => void;
    onauth: (e: unknown) => boolean;
    onerror: (text: string) => void;
  } = $props();

  // ---- editor dialog ------------------------------------------------------

  let editing = $state<"add" | "edit" | null>(null);
  let formName = $state("");
  let formProxy = $state("");
  let formApiKey = $state("");
  let formModels = $state("");
  let formKeyTouched = $state(false);
  let busy = $state(false);
  let outcome = $state<OutcomeBox | null>(null);
  let outcomeError = $state<string | null>(null);

  // ---- remove dialog ------------------------------------------------------

  let removing = $state<PeerState | null>(null);
  let removingBusy = $state(false);

  function message(e: unknown): string {
    return e instanceof Error ? e.message : String(e);
  }

  function openAdd(): void {
    editing = "add";
    formName = "";
    formProxy = "";
    formApiKey = "";
    formModels = "";
    formKeyTouched = false;
    outcome = null;
    outcomeError = null;
  }

  function openEdit(peer: PeerState): void {
    editing = "edit";
    formName = peer.name;
    formProxy = peer.proxy;
    formApiKey = peer.apiKey.macro ?? "";
    formModels = modelsToText(peer.models);
    formKeyTouched = peer.apiKey.set;
    outcome = null;
    outcomeError = null;
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
        apiKey: formApiKey,
        models: modelsFromText(formModels),
      };
    }
    return {
      approved,
      name: formName.trim(),
      proxy: formProxy.trim(),
      // Only send the key when it was edited; "" alone would clear it.
      apiKey: formKeyTouched ? formApiKey : undefined,
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
        <Label.Root for="peer-proxy" class="text-sm">Proxy URL</Label.Root>
        <Input id="peer-proxy" bind:value={formProxy} placeholder="https://openrouter.ai/api" class="mt-1" />
      </div>
      <div>
        <Label.Root for="peer-api-key" class="text-sm">API key (env macro only)</Label.Root>
        <Input
          id="peer-api-key"
          bind:value={formApiKey}
          oninput={() => (formKeyTouched = true)}
          placeholder={'${env.MY_PEER_KEY}'}
          class="mt-1 font-mono"
        />
        <p class="text-muted-foreground mt-1 text-xs">
          {#if keyError()}
            <span class="text-destructive">{keyError()}</span>
          {:else}
            An {'${env.VAR}'} reference the server resolves at startup. Literal secrets are rejected.
          {/if}
        </p>
      </div>
      <div>
        <Label.Root for="peer-models" class="text-sm">Models</Label.Root>
        <Textarea
          id="peer-models"
          bind:value={formModels}
          rows={2}
          placeholder="llama-3.1-8b, deepseek-chat"
          class="mt-1"
        />
        <p class="text-muted-foreground mt-1 text-xs">Comma or newline separated model IDs.</p>
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
