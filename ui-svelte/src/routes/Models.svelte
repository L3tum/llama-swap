<script lang="ts">
  import { isNarrow } from "../stores/theme";
  import { models, modelLogs, selectedModel, toggleModelSelection, upstreamLogs } from "../stores/api";
  import ModelsPanel from "../components/ModelsPanel.svelte";
  import LogPanel from "../components/LogPanel.svelte";
  import ResizablePanels from "../components/ResizablePanels.svelte";

  let direction = $derived<"horizontal" | "vertical">($isNarrow ? "vertical" : "horizontal");
  let activeLogData = $derived($selectedModel ? ($modelLogs[$selectedModel] ?? "") : $upstreamLogs);
  let activeLogTitle = $derived.by(() => {
    if (!$selectedModel) return "Upstream Logs";
    const model = $models.find((m) => m.id === $selectedModel);
    return `Logs: ${model?.name || model?.id || $selectedModel}`;
  });

  function clearModelSelection(): void {
    selectedModel.set(null);
  }
</script>

<ResizablePanels {direction} storageKey="models-panel-group">
  {#snippet leftPanel()}
    <ModelsPanel selectedModel={$selectedModel} onSelectModel={toggleModelSelection} />
  {/snippet}
  {#snippet rightPanel()}
    <LogPanel
      id="modelsupstream"
      title={activeLogTitle}
      logData={activeLogData}
      onClear={$selectedModel ? clearModelSelection : undefined}
    />
  {/snippet}
</ResizablePanels>
