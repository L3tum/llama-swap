<script lang="ts">
  import type { Model } from "../lib/types";
  import { lockModel, unlockModel } from "../stores/api";
  import { Lock, LockOpen } from "@lucide/svelte";

  interface Props {
    model: Model;
    /** "md" for list rows (size-7), "sm" for the detail header (size-5). */
    size?: "md" | "sm";
    /** True while the global lock is engaged: every model is effectively
     * locked, so per-model toggles are no-ops and the button is disabled
     * rather than allowing a click that changes nothing. */
    globalLocked?: boolean;
  }

  let { model, size = "md", globalLocked = false }: Props = $props();

  let btnSize = $derived(size === "sm" ? "size-5 rounded-sm" : "size-7 rounded-md");
  let iconSize = $derived(size === "sm" ? "size-3.5" : "size-4");
  let locked = $derived(!!model.locked);
  let pending = $state(false);

  // The server pushes the new state on the next modelStatus; pending only
  // guards against double-firing while the request is in flight.
  async function toggle(): Promise<void> {
    if (pending || globalLocked) return;
    pending = true;
    try {
      if (locked) {
        await unlockModel(model.id);
      } else {
        await lockModel(model.id);
      }
    } catch (e) {
      console.error(e);
    } finally {
      pending = false;
    }
  }
</script>

<button
  type="button"
  class={`flex ${btnSize} shrink-0 items-center justify-center disabled:opacity-50 ${
    locked
      ? "text-foreground hover:bg-accent"
      : "text-muted-foreground hover:bg-accent hover:text-accent-foreground"
  }`}
  title={globalLocked ? "Locked by global lock" : locked ? "Unlock model" : "Lock model"}
  aria-label={globalLocked ? "Model locked by global lock" : locked ? "Unlock model" : "Lock model"}
  disabled={globalLocked || pending}
  onclick={toggle}
>
  {#if locked}
    <Lock class={iconSize} />
  {:else}
    <LockOpen class={iconSize} />
  {/if}
</button>
