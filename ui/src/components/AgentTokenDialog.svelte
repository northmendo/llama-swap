<script lang="ts">
  /**
   * Modal that asks for the /agent/v1 bearer token. Shown on first use
   * of the engines/discovery views and again on any 401. The token is
   * stored in localStorage (stores/agentApiToken) and can be cleared
   * here or from the dialog itself. The key is never bundled or
   * committed anywhere.
   */
  import { Button } from "$lib/components/ui/button/index.js";
  import * as Dialog from "$lib/components/ui/dialog/index.js";
  import * as Label from "$lib/components/ui/label/index.js";
  import { Input } from "$lib/components/ui/input/index.js";
  import { KeyRound } from "@lucide/svelte";
  import { setAgentToken, clearAgentToken } from "../stores/agentApiToken";

  interface Props {
    /** Whether the dialog is open (bind:open). */
    open: boolean;
    /** Why the prompt is shown; "" is the first-use case. */
    reason?: string;
    /** Called after a token was saved. */
    onsaved?: () => void;
  }

  let { open = $bindable(false), reason = "", onsaved }: Props = $props();

  let value = $state("");

  function handleSave(): void {
    setAgentToken(value);
    value = "";
    open = false;
    onsaved?.();
  }

  function handleClear(): void {
    clearAgentToken();
    value = "";
  }

  function handleOpenChange(v: boolean): void {
    open = v;
  }
</script>

<Dialog.Root {open} onOpenChange={handleOpenChange}>
  <Dialog.Content class="sm:max-w-md">
    <Dialog.Header>
      <Dialog.Title class="flex items-center gap-2">
        <KeyRound class="size-5" />
        Agent API key
      </Dialog.Title>
      <Dialog.Description>
        {reason
          ? reason
          : "The engines and discovery views talk to llama-swap's agent API (/agent/v1), which needs its bearer key from the agent_api section of the config. Enter it once; it is stored in this browser only."}
      </Dialog.Description>
    </Dialog.Header>

    <form
      class="space-y-3"
      onsubmit={(e) => {
        e.preventDefault();
        handleSave();
      }}
    >
      <div class="space-y-2">
        <Label.Root for="agent-api-token">Bearer token</Label.Root>
        <Input
          id="agent-api-token"
          type="password"
          autocomplete="off"
          spellcheck={false}
          bind:value
          placeholder="the agent_api key of this server"
        />
      </div>

      <div class="flex flex-col-reverse gap-2 sm:flex-row sm:justify-end">
        <Button type="button" variant="outline" onclick={handleClear}>Clear saved key</Button>
        <Button type="submit" disabled={!value.trim()}>Save</Button>
      </div>
    </form>
  </Dialog.Content>
</Dialog.Root>
