<script lang="ts">
  /**
   * Searchable model pick list for the Config view's forms. Pure
   * presentation: the parent owns loading, errors and the option list
   * (from the live provider catalog, local models, or both); this
   * component renders the search box, the filtered rows and the
   * loading/empty/error states. One dead provider shows one inline
   * error here and never breaks the panel around it.
   */
  import { Input } from "$lib/components/ui/input/index.js";
  import { Search } from "@lucide/svelte";
  import { filterPickOptions, type ModelPickOption } from "../../lib/configUi";

  let {
    options = [],
    loading = false,
    error = "",
    emptyText = "No models to show.",
    placeholder = "Search models…",
    onpick,
  }: {
    options?: ModelPickOption[];
    loading?: boolean;
    error?: string;
    emptyText?: string;
    placeholder?: string;
    onpick: (id: string) => void;
  } = $props();

  let query = $state("");
  let filtered = $derived(filterPickOptions(options, query));
</script>

<div class="space-y-1.5">
  <div class="relative">
    <Search class="text-muted-foreground pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2" />
    <Input
      class="pl-8 text-xs"
      bind:value={query}
      {placeholder}
      aria-label="Filter models"
      disabled={loading || options.length === 0}
    />
  </div>

  {#if error}
    <p class="text-destructive rounded-md border border-destructive/40 bg-destructive/10 p-2 text-xs">
      {error}
    </p>
  {:else if loading}
    <p class="text-muted-foreground p-2 text-xs">Loading models from the provider…</p>
  {:else if filtered.length === 0}
    <p class="text-muted-foreground p-2 text-xs">{emptyText}</p>
  {:else}
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
  {/if}
</div>
