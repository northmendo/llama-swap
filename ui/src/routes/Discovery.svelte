<script lang="ts">
  /**
   * Discovery view: scan the configured model folders, inspect every
   * GGUF the scanner finds, and draft/validate/apply model stanzas.
   * All data comes from the bearer-authenticated /agent/v1 API:
   *
   *   POST /agent/v1/scan      "Scan models"
   *   POST /agent/v1/validate  per-draft validation (no side effects)
   *   POST /agent/v1/refine    LLM-assisted refinement, when configured
   *                            (a 400 hides the button)
   *   POST /agent/v1/apply     merge the stanza into the config file,
   *                            only after the confirm dialog
   */
  import { Button } from "$lib/components/ui/button/index.js";
  import * as Dialog from "$lib/components/ui/dialog/index.js";
  import { FolderSearch, FileCheck2, Sparkles, Plus } from "@lucide/svelte";
  import AgentTokenDialog from "../components/AgentTokenDialog.svelte";
  import EmptyState from "../components/EmptyState.svelte";
  import Tag from "../components/Tag.svelte";
  import { basename, formatBytes, formatContext } from "../lib/modelFiles";
  import {
    AgentApiError,
    AgentUnauthorizedError,
    applyModel,
    currentToken,
    refineDraft,
    scanModels,
    validateYaml,
    type ApplyResponse,
    type ModelDraft,
    type RefineResponse,
    type ScanFileResult,
    type ScanResponse,
    type ValidationResponse,
  } from "../lib/agentApi";

  let scan = $state<ScanResponse | null>(null);
  let scanning = $state(false);
  let error = $state<string | null>(null);

  /** The 400 refine response means no draft assistant: hide the buttons. */
  let refineConfigured = $state(true);

  interface FileState {
    validating?: boolean;
    validation?: ValidationResponse;
    refining?: boolean;
    refine?: RefineResponse;
    applyError?: string;
    applyConflict?: string;
    applied?: ApplyResponse;
    /** The draft the actions work on (the refined one after a refine). */
    draft: ModelDraft;
    draftOpen?: boolean;
    busy?: boolean;
  }

  let fileStates = $state<Record<string, FileState>>({});

  let tokenOpen = $state(false);
  let tokenReason = $state("");

  // The Add-model confirmation dialog.
  let addOpen = $state(false);
  let addFile = $state<ScanFileResult | null>(null);

  function message(e: unknown): string {
    return e instanceof Error ? e.message : String(e);
  }

  function openTokenPrompt(reason: string): void {
    tokenReason = reason;
    tokenOpen = true;
  }

  function authProblem(e: unknown): boolean {
    if (e instanceof AgentUnauthorizedError) {
      openTokenPrompt("The saved key was rejected (401). Enter the agent_api key of this server.");
      return true;
    }
    return false;
  }

  async function doScan(): Promise<void> {
    if (!currentToken()) {
      openTokenPrompt("");
      return;
    }
    scanning = true;
    error = null;
    try {
      scan = await scanModels();
      fileStates = {};
      for (const file of scan.results) {
        if (file.draft) {
          fileStates[file.path] = { draft: file.draft };
        }
      }
    } catch (e) {
      if (!authProblem(e)) error = message(e);
    } finally {
      scanning = false;
    }
  }

  function stateFor(file: ScanFileResult): FileState | undefined {
    return fileStates[file.path];
  }

  /** POST /agent/v1/validate with the (possibly refined) draft yaml. */
  async function doValidate(file: ScanFileResult): Promise<void> {
    const state = stateFor(file);
    if (!state) return;
    state.validating = true;
    state.validation = undefined;
    try {
      state.validation = await validateYaml(state.draft.yaml);
    } catch (e) {
      if (!authProblem(e)) state.validation = { valid: false, errors: [message(e)] };
    } finally {
      state.validating = false;
    }
  }

  /**
   * POST /agent/v1/refine. A 400 means refinement is not configured on
   * this server: hide the button everywhere.
   */
  async function doRefine(file: ScanFileResult): Promise<void> {
    const state = stateFor(file);
    if (!state) return;
    state.refining = true;
    try {
      const refined = await refineDraft(file.path);
      state.refine = refined;
      state.draft = refined;
      state.validation = undefined;
      // The refined draft must still validate before Add is offered.
      state.validation = await validateYaml(refined.yaml);
    } catch (e) {
      if (e instanceof AgentApiError && e.status === 400) {
        refineConfigured = false;
      } else if (!authProblem(e)) {
        state.refine = {
          ...state.draft,
          ...{ refined: false, reason: message(e) },
        } as RefineResponse;
      }
    } finally {
      state.refining = false;
    }
  }

  function openAdd(file: ScanFileResult): void {
    addFile = file;
    addOpen = true;
  }

  /** The confirmed apply: POST /agent/v1/apply with approved: true. */
  async function doApply(): Promise<void> {
    const file = addFile;
    addOpen = false;
    if (!file) return;
    const state = stateFor(file);
    if (!state) return;
    state.busy = true;
    state.applyError = undefined;
    state.applyConflict = undefined;
    try {
      state.applied = await applyModel(state.draft.yaml, true);
    } catch (e) {
      if (!authProblem(e)) {
        if (e instanceof AgentApiError && e.status === 409) {
          state.applyConflict = message(e);
        } else {
          state.applyError = message(e);
        }
      }
    } finally {
      state.busy = false;
    }
  }

  /** Add is offered once the (refined, when present) draft validates. */
  function addEnabled(state: FileState): boolean {
    if (state.applied || state.applyConflict) return false;
    if (state.refine) return state.validation?.valid === true;
    return true;
  }
</script>

<div class="mx-auto max-w-3xl space-y-4 p-2">
  <div class="flex flex-wrap items-center gap-2">
    <h3 class="text-lg font-semibold">Discovery</h3>
    <Button class="ml-auto" size="sm" onclick={doScan} disabled={scanning}>
      <FolderSearch />
      {scan ? "Rescan" : "Scan models"}
    </Button>
  </div>

  {#if error}
    <div class="text-destructive rounded-lg border border-destructive/40 bg-destructive/10 p-3 text-sm">
      {error}
    </div>
  {/if}

  {#if scanning}
    <EmptyState message="Scanning model folders…" />
  {:else if !scan}
    <EmptyState message="Scan the folders from the config's discovery section to see every GGUF file, its metadata, and a draft model stanza for each." />
  {:else if scan.results.length === 0}
    <EmptyState message="No model files found in the configured discovery folders." />
  {:else}
    <div class="text-muted-foreground flex flex-wrap gap-2 text-xs">
      <Tag>{scan.found} found</Tag>
      <Tag>{scan.drafted} drafted</Tag>
      <Tag>{scan.skipped} skipped</Tag>
    </div>

    <div class="space-y-3">
      {#each scan.results as file (file.path)}
        {@const state = stateFor(file)}
        <div class="space-y-2 rounded-lg border p-4">
          <div class="flex flex-wrap items-center gap-2">
            <span class="min-w-0 truncate font-mono text-sm font-semibold" title={file.path}>
              {basename(file.path)}
            </span>
            {#if file.status === "drafted"}
              <Tag class="bg-success/15 text-success">drafted</Tag>
            {:else}
              <Tag class="bg-warning/15 text-warning">skipped</Tag>
            {/if}
            {#if file.draft}
              <Tag class="ml-auto">engine: {file.draft.engine}{#if file.draft.engineSource} · {file.draft.engineSource}{/if}</Tag>
            {/if}
          </div>

          {#if file.status !== "drafted" && file.skipReason}
            <p class="text-muted-foreground text-xs">{file.skipReason}</p>
          {/if}

          {#if file.metadataError}
            <p class="text-warning text-xs">metadata: {file.metadataError}</p>
          {/if}

          {#if file.metadata}
            <dl class="text-muted-foreground grid grid-cols-2 gap-x-4 gap-y-1 text-xs sm:grid-cols-4">
              <div>
                <dt>architecture</dt>
                <dd class="font-medium text-foreground">{file.metadata.architecture || "—"}</dd>
              </div>
              <div>
                <dt>context</dt>
                <dd class="font-medium text-foreground">
                  {file.draft
                    ? formatContext(file.draft.ctxSize, file.draft.ctxCapped)
                    : (file.metadata.contextLength || 0).toLocaleString()}
                </dd>
              </div>
              <div>
                <dt>quant</dt>
                <dd class="font-medium text-foreground">{file.metadata.quantHint || "—"}</dd>
              </div>
              <div>
                <dt>size</dt>
                <dd class="font-medium text-foreground">{formatBytes(file.metadata.fileSize)}</dd>
              </div>
            </dl>
          {/if}

          {#if state}
            {#if state.refine}
              <p class={`text-xs ${state.refine.refined ? "text-success" : "text-muted-foreground"}`}>
                {state.refine.refined
                  ? "refined by the draft assistant"
                  : `refinement fell back to the rule-based draft${state.refine.reason ? `: ${state.refine.reason}` : ""}`}
              </p>
            {/if}

            <details
              open={state.draftOpen}
              ontoggle={(e) => (state.draftOpen = (e.currentTarget as HTMLDetailsElement).open)}
            >
              <summary class="text-muted-foreground cursor-pointer text-xs hover:text-foreground">
                Draft stanza — {state.draft.modelId}
              </summary>
              <pre class="bg-muted mt-2 max-h-72 overflow-auto rounded-md p-2 font-mono text-xs">{state.draft.yaml}</pre>
            </details>

            <div class="flex flex-wrap gap-2">
              <Button variant="outline" size="sm" onclick={() => doValidate(file)} disabled={state.validating || state.busy}>
                <FileCheck2 />
                Validate
              </Button>
              {#if refineConfigured}
                <Button variant="outline" size="sm" onclick={() => doRefine(file)} disabled={state.refining || state.busy}>
                  <Sparkles class={state.refining ? "animate-pulse" : ""} />
                  Refine draft
                </Button>
              {/if}
              <Button variant="outline" size="sm" onclick={() => openAdd(file)} disabled={!addEnabled(state) || state.busy}>
                <Plus />
                Add model
              </Button>
            </div>

            {#if state.validation}
              <div
                class={`rounded-md border p-2 text-xs ${state.validation.valid
                  ? "border-success/40 bg-success/10"
                  : "border-destructive/40 bg-destructive/10"}`}
              >
                {#if state.validation.valid}
                  <span class="text-success font-medium">draft validates</span>
                {:else}
                  <span class="text-destructive font-medium">draft does not validate</span>
                  <ul class="text-destructive mt-1 list-disc space-y-0.5 pl-4">
                    {#each state.validation.errors as err (err)}
                      <li>{err}</li>
                    {/each}
                  </ul>
                {/if}
              </div>
            {/if}

            {#if state.applied}
              <div class="border-success/40 bg-success/10 rounded-md border p-2 text-xs">
                <span class="text-success font-medium">
                  {state.applied.status === "applied" ? "model added" : state.applied.status}
                  — {state.applied.model}
                </span>
                <ul class="text-muted-foreground mt-1 list-disc space-y-0.5 pl-4">
                  {#if state.applied.backup}<li>backup: <span class="font-mono">{state.applied.backup}</span></li>{/if}
                  <li>
                    {state.applied.reloadRequired
                      ? "reload required: send SIGHUP (or restart) to load the model, or run with --watch-config"
                      : "--watch-config will pick the new model up automatically"}
                  </li>
                </ul>
              </div>
            {/if}

            {#if state.applyConflict}
              <div class="border-warning/40 bg-warning/10 rounded-md border p-2 text-xs">
                <span class="text-warning font-medium">duplicate model</span>
                <p class="text-muted-foreground mt-1">{state.applyConflict}</p>
              </div>
            {/if}

            {#if state.applyError}
              <div class="border-destructive/40 bg-destructive/10 rounded-md border p-2 text-xs">
                <span class="text-destructive font-medium">apply failed</span>
                <p class="text-muted-foreground mt-1">{state.applyError}</p>
              </div>
            {/if}
          {/if}
        </div>
      {/each}
    </div>
  {/if}
</div>

<Dialog.Root bind:open={addOpen} onOpenChange={(v) => (addOpen = v)}>
  <Dialog.Content class="sm:max-w-lg">
    <Dialog.Header>
      <Dialog.Title>
        Add model
        <span class="text-muted-foreground font-mono text-sm font-normal">{addFile && stateFor(addFile)?.draft.modelId}</span>
      </Dialog.Title>
      <Dialog.Description>
        The stanza below is merged into the config file's models section. A timestamped backup is
        taken first; the merged config is fully validated before anything is written.
      </Dialog.Description>
    </Dialog.Header>

    {#if addFile && stateFor(addFile)}
      <pre class="bg-muted max-h-72 overflow-auto rounded-md p-2 font-mono text-xs">{stateFor(addFile)!.draft.yaml}</pre>
    {/if}

    <Dialog.Footer>
      <Button variant="outline" onclick={() => (addOpen = false)}>Cancel</Button>
      <Button onclick={doApply}>
        <Plus />
        Add to config
      </Button>
    </Dialog.Footer>
  </Dialog.Content>
</Dialog.Root>

<AgentTokenDialog
  bind:open={tokenOpen}
  reason={tokenReason}
  onsaved={() => void doScan()}
/>
