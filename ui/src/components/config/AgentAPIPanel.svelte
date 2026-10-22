<script lang="ts">
  /**
   * Agent API panel of the Config view: the agent_api: section — the
   * enabled toggle (disabling warns that the /agent/v1 routes unmount
   * on reload), the key macro display/rotate (an ${env.VAR} reference
   * ONLY, never a literal secret) and the timestamped config backups
   * with one-click restore behind a confirmation.
   */
  import { Button } from "$lib/components/ui/button/index.js";
  import * as Dialog from "$lib/components/ui/dialog/index.js";
  import * as Label from "$lib/components/ui/label/index.js";
  import * as Switch from "$lib/components/ui/switch/index.js";
  import { Input } from "$lib/components/ui/input/index.js";
  import { KeyRound, RotateCcw, History } from "@lucide/svelte";
  import EmptyState from "../EmptyState.svelte";
  import Tag from "../Tag.svelte";
  import {
    type AgentAPIState,
    type ConfigBackup,
    restoreConfigBackup,
    updateAgentAPI,
  } from "../../lib/agentApi";
  import {
    backupRow,
    describeWriteResult,
    envMacroError,
    secretLabel,
    type OutcomeBox,
  } from "../../lib/configUi";
  import { formatTimestamp } from "../../lib/engineUpdates";

  let {
    agentApi = null,
    backups = [],
    onchanged,
    onauth,
    onerror,
  }: {
    agentApi?: AgentAPIState | null;
    backups?: ConfigBackup[];
    onchanged: () => void;
    onauth: (e: unknown) => boolean;
    onerror: (text: string) => void;
  } = $props();

  // ---- enabled toggle -----------------------------------------------------

  let toggling = $state(false);
  let disableOpen = $state(false);

  // ---- key rotate dialog ---------------------------------------------------

  let keyOpen = $state(false);
  let formKey = $state("");
  let busy = $state(false);
  let outcome = $state<OutcomeBox | null>(null);
  let outcomeError = $state<string | null>(null);

  // ---- restore dialog -----------------------------------------------------

  let restoring = $state<ConfigBackup | null>(null);
  let restoringBusy = $state(false);

  function message(e: unknown): string {
    return e instanceof Error ? e.message : String(e);
  }

  async function runEnabled(enabled: boolean): Promise<void> {
    toggling = true;
    try {
      await updateAgentAPI({ approved: true, enabled });
      onchanged();
    } catch (e) {
      if (!onauth(e)) onerror(`could not set agent_api.enabled: ${message(e)}`);
    } finally {
      toggling = false;
    }
  }

  function openKeyRotate(): void {
    formKey = "";
    outcome = null;
    outcomeError = null;
    keyOpen = true;
  }

  function keyError(): string | null {
    return envMacroError("key", formKey);
  }

  async function runKey(approved: boolean): Promise<void> {
    outcome = null;
    outcomeError = null;
    busy = true;
    try {
      const res = await updateAgentAPI({ approved, key: formKey });
      outcome = describeWriteResult(res);
      if (res.written) {
        keyOpen = false;
        onchanged();
      }
    } catch (e) {
      if (!onauth(e)) outcomeError = message(e);
    } finally {
      busy = false;
    }
  }

  async function runRestore(): Promise<void> {
    if (!restoring) return;
    restoringBusy = true;
    try {
      await restoreConfigBackup(restoring.name, true);
      restoring = null;
      onchanged();
    } catch (e) {
      if (!onauth(e)) onerror(`could not restore ${restoring?.name}: ${message(e)}`);
    } finally {
      restoringBusy = false;
    }
  }
</script>

<div class="space-y-4">
  {#if !agentApi}
    <EmptyState
      message="No agent_api: section is set in the config. Add one (enabled + key) to use these endpoints."
    />
  {:else}
    <section class="space-y-3 rounded-lg border p-4">
      <div class="flex flex-wrap items-center gap-2">
        <h4 class="text-sm font-semibold">Agent API</h4>
        <div class="ml-auto flex items-center gap-3">
          <span class="text-muted-foreground text-xs">{agentApi.enabled ? "enabled" : "disabled"}</span>
          <Switch.Root
            id="agent-api-enabled"
            checked={agentApi.enabled}
            disabled={toggling}
            onCheckedChange={(v) => {
              if (v) void runEnabled(true);
              else disableOpen = true;
            }}
          />
        </div>
      </div>
      <p class="text-muted-foreground text-xs">
        The /agent/v1 endpoints this UI (and any agent) authenticates to with a bearer token.
        {#if agentApi.enabled}
          Disabling unmounts every /agent/v1 route after the next reload.
        {:else}
          Nothing is mounted while disabled; enabling requires a restart or SIGHUP reload.
        {/if}
      </p>

      <div class="flex flex-wrap items-center gap-2 rounded-md border p-3">
        <KeyRound class="text-muted-foreground size-4" />
        <span class="text-muted-foreground text-xs">key:</span>
        <span class="font-mono text-xs">{secretLabel(agentApi.key)}</span>
        <div class="ml-auto">
          <Button variant="outline" size="sm" onclick={openKeyRotate}>
            <RotateCcw />
            Rotate
          </Button>
        </div>
      </div>
      <p class="text-muted-foreground text-xs">
        The key itself never reaches the browser: the config stores an environment-variable
        reference the server resolves at startup.
      </p>
    </section>
  {/if}

  <section class="space-y-3 rounded-lg border p-4">
    <div class="flex flex-wrap items-center gap-2">
      <h4 class="flex items-center gap-2 text-sm font-semibold">
        <History class="size-4" />
        Config backups
      </h4>
      <span class="text-muted-foreground text-xs">taken by every approved change and restore</span>
    </div>

    {#if backups.length === 0}
      <p class="text-muted-foreground text-xs">No backups yet — they appear after the first approved change.</p>
    {:else}
      <div class="overflow-x-auto rounded-lg border">
        <table class="w-full text-sm">
          <thead>
            <tr class="text-muted-foreground border-b text-left text-xs uppercase">
              <th class="px-3 py-2">Backup</th>
              <th class="px-3 py-2">Size</th>
              <th class="px-3 py-2">Modified</th>
              <th class="px-3 py-2"></th>
            </tr>
          </thead>
          <tbody>
            {#each backups as backup (backup.name)}
              {@const row = backupRow(backup)}
              <tr class="border-b last:border-b-0">
                <td class="px-3 py-2"><Tag>{row.name}</Tag></td>
                <td class="text-muted-foreground px-3 py-2 text-xs">{row.size}</td>
                <td class="text-muted-foreground px-3 py-2 text-xs">{formatTimestamp(row.modified)}</td>
                <td class="px-3 py-2 text-right">
                  <Button variant="outline" size="sm" onclick={() => (restoring = backup)}>
                    Restore
                  </Button>
                </td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    {/if}
  </section>
</div>

<Dialog.Root
  open={keyOpen}
  onOpenChange={(v) => {
    if (!v) keyOpen = false;
  }}
>
  <Dialog.Content class="sm:max-w-md">
    <Dialog.Header>
      <Dialog.Title>Rotate the agent API key</Dialog.Title>
      <Dialog.Description>
        Point the config at a different environment variable. The reference is validated first;
        Apply writes it and takes a backup. The resolved key is never shown or sent.
      </Dialog.Description>
    </Dialog.Header>

    <div>
      <Label.Root for="agent-key" class="text-sm">Key (env macro only)</Label.Root>
      <Input
        id="agent-key"
        bind:value={formKey}
        class="mt-1 font-mono"
        placeholder={'${env.MY_AGENT_KEY}'}
      />
      <p class="text-muted-foreground mt-1 text-xs">
        {#if keyError()}
          <span class="text-destructive">{keyError()}</span>
        {:else}
          An environment-variable reference the server resolves at startup. Literal secrets are
          rejected.
        {/if}
      </p>
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
      <Button variant="outline" onclick={() => (keyOpen = false)}>Cancel</Button>
      <Button variant="outline" disabled={busy || !!keyError()} onclick={() => void runKey(false)}>
        Validate
      </Button>
      <Button disabled={busy || !!keyError()} onclick={() => void runKey(true)}>Apply</Button>
    </Dialog.Footer>
  </Dialog.Content>
</Dialog.Root>

<Dialog.Root
  open={disableOpen}
  onOpenChange={(v) => {
    if (!v) disableOpen = false;
  }}
>
  <Dialog.Content class="sm:max-w-md">
    <Dialog.Header>
      <Dialog.Title>Disable the agent API?</Dialog.Title>
      <Dialog.Description>
        agent_api.enabled=false is written to the config. After the next reload every /agent/v1
        route — including the one this UI uses — is unmounted, and editing the config file is the
        only way to re-enable it.
      </Dialog.Description>
    </Dialog.Header>
    <Dialog.Footer>
      <Button variant="outline" onclick={() => (disableOpen = false)}>Cancel</Button>
      <Button
        variant="destructive"
        onclick={() => {
          disableOpen = false;
          void runEnabled(false);
        }}
      >
        Disable
      </Button>
    </Dialog.Footer>
  </Dialog.Content>
</Dialog.Root>

<Dialog.Root
  open={restoring !== null}
  onOpenChange={(v) => {
    if (!v) restoring = null;
  }}
>
  <Dialog.Content class="sm:max-w-md">
    <Dialog.Header>
      <Dialog.Title>Restore {restoring?.name ?? ""}?</Dialog.Title>
      <Dialog.Description>
        The backup's content is validated first, then written over the current config (which is
        itself backed up, so a restore is reversible). A reload or restart loads it.
      </Dialog.Description>
    </Dialog.Header>
    <Dialog.Footer>
      <Button variant="outline" onclick={() => (restoring = null)}>Cancel</Button>
      <Button variant="destructive" disabled={restoringBusy} onclick={() => void runRestore()}>
        Restore
      </Button>
    </Dialog.Footer>
  </Dialog.Content>
</Dialog.Root>
