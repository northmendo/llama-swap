<script lang="ts">
  /**
   * Searchable model pick list for the Config view's forms. Pure
   * presentation: the parent owns loading, errors and the option list
   * (from the server's cached provider catalogs, local models, or both);
   * this component renders the search box, the filtered rows — grouped by
   * source when the caller passes groups — and the loading/empty/error
   * states. One dead provider shows one inline error here and never
   * breaks the panel around it.
   */
  import { Input } from "$lib/components/ui/input/index.js";
  import { Search } from "@lucide/svelte";
  import { filterPickOptions, filterPickGroups, type ModelPickOption, type PickGroup } from "../../lib/configUi";

  let {
    options = [],
    groups,
    loading = false,
    error = "",
    emptyText = "No models to show.",
    placeholder = "Search models…",
    onpick,
  }: {
    /** A single flat list. Ignored when `groups` is set. */
    options?: ModelPickOption[];
    /** Grouped options: one section per source (provider, this server). */
    groups?: PickGroup[];
    loading?: boolean;
    error?: string;
    emptyText?: string;
    placeholder?: string;
    onpick: (id: string) => void;
  } = $props();

  let query = $state("");
  let filtered = $derived(groups ? [] : filterPickOptions(options, query));
  let filteredGroups = $derived(groups ? filterPickGroups(groups, query) : []);
  let total = $derived(
    groups ? groups.reduce((sum, group) => sum + group.options.length, 0) : options.length,
  );
</script>

<div class="space-y-1.5">
  <div class="relative">
    <Search class="text-muted-foreground pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2" />
    <Input
      class="pl-8 text-xs"
      bind:value={query}
      {placeholder}
      aria-label="Filter models"
      disabled={loading || total === 0}
    />
  </div>

  {#if error}
    <p class="text-destructive rounded-md border border-destructive/40 bg-destructive/10 p-2 text-xs">
      {error}
    </p>
  {:else if loading}
    <p class="text-muted-foreground p-2 text-xs">Loading models from the provider…</p>
  {:else if groups && filteredGroups.length > 0}
    <div class="max-h-56 overflow-y-auto rounded-md border text-xs">
      {#each filteredGroups as group (group.label)}
        <div class="border-b last:border-b-0">
          <div class="bg-muted/60 text-muted-foreground px-2.5 py-1 text-[10px] font-medium uppercase">
            {group.label}
          </div>
          <ul>
            {#each group.options as opt (opt.id)}
              <li class="border-t first:border-t-0">
                <button
                  type="button"
                  class="hover:bg-accent w-full px-2.5 py-1.5 text-left"
                  onclick={() => onpick(opt.id)}
                >
                  <span class="block font-mono">{opt.id}</span>
                  {#if opt.hint}
                    <span class="text-muted-foreground block text-[10px]">{opt.hint}</span>
                  {/if}
                </button>
              </li>
            {/each}
          </ul>
        </div>
      {/each}
    </div>
  {:else if !groups && filtered.length > 0}
    <ul class="max-h-48 overflow-y-auto rounded-md border text-xs">
      {#each filtered as opt (opt.id)}
        <li class="border-b last:border-b-0">
          <button
            type="button"
            class="hover:bg-accent w-full px-2.5 py-1.5 text-left"
            onclick={() => onpick(opt.id)}
          >
            <span class="block font-mono">{opt.id}</span>
            {#if opt.hint}
              <span class="text-muted-foreground block text-[10px]">{opt.hint}</span>
            {/if}
          </button>
        </li>
      {/each}
    </ul>
  {:else}
    <p class="text-muted-foreground p-2 text-xs">{emptyText}</p>
  {/if}
</div>
