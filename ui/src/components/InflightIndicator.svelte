<script lang="ts">
  import { Zap } from "@lucide/svelte";
  import { inflightRequestEntries } from "../stores/api";
  import { inflightHistory } from "../stores/inflightActivity";

  const WINDOW_MS = 60_000;
  const WIDTH = 44;
  const HEIGHT = 12;

  interface Props {
    modelId: string;
  }
  let { modelId }: Props = $props();

  let count = $derived(
    $inflightRequestEntries.reduce((n, e) => (e.model === modelId ? n + 1 : n), 0),
  );
  let series = $derived($inflightHistory[modelId]);

  // Re-render every few seconds so the sparkline window slides and the
  // "recent activity" state expires even when no new events arrive.
  let now = $state(Date.now());
  $effect(() => {
    const id = setInterval(() => (now = Date.now()), 5_000);
    return () => clearInterval(id);
  });

  let recent = $derived((series ?? []).filter((p) => now - p.t <= WINDOW_MS));
  let show = $derived(count > 0 || recent.some((p) => p.count > 0));
  let points = $derived.by(() => {
    if (recent.length < 2) return null;
    const max = Math.max(1, ...recent.map((p) => p.count));
    return recent
      .map((p) => {
        const x = WIDTH - ((now - p.t) / WINDOW_MS) * WIDTH;
        const y = HEIGHT - 1 - (p.count / max) * (HEIGHT - 2);
        return `${x.toFixed(1)},${y.toFixed(1)}`;
      })
      .join(" ");
  });
</script>

{#if show}
  <span
    class="flex items-center gap-1 text-xs tabular-nums {count > 0 ? '' : 'text-muted-foreground'}"
    title="{count} in-flight request{count === 1 ? '' : 's'}"
  >
    {#if count > 0}
      <Zap class="size-3" />
      <span>{count}</span>
    {/if}
    {#if points}
      <svg
        width={WIDTH}
        height={HEIGHT}
        class="shrink-0 text-primary/70"
        aria-hidden="true"
      >
        <polyline
          points={points}
          fill="none"
          stroke="currentColor"
          stroke-width="1"
          stroke-linecap="round"
          stroke-linejoin="round"
        />
      </svg>
    {/if}
  </span>
{/if}
