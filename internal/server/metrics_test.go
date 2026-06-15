package server

import (
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/activitylog"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/tidwall/gjson"
)

func TestServer_ClearMetricsClearsCaptureCache(t *testing.T) {
	mm := newMetricsMonitor(logmon.NewWriter(io.Discard), 10, 1, nil, 0)
	if !mm.addCapture(ReqRespCapture{ID: 1, ReqPath: "/v1/chat/completions"}) {
		t.Fatal("addCapture returned false")
	}
	if capture := mm.getCaptureByID(1); capture == nil {
		t.Fatal("capture missing before clear")
	}

	mm.clearMetrics(0)

	if capture := mm.getCaptureByID(1); capture != nil {
		t.Fatal("capture still available after clear")
	}
}

func TestServer_MetricsDBPrunesOldEntries(t *testing.T) {
	f, err := os.CreateTemp("", "llama-swap-metrics-prune-*.db")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	path := f.Name()
	f.Close()
	defer os.Remove(path)

	store, err := activitylog.New(path)
	if err != nil {
		t.Fatalf("activitylog.New: %v", err)
	}
	mm := newMetricsMonitor(logmon.NewWriter(io.Discard), 10, 0, store, 3)
	defer mm.Close()

	for i := 0; i < 6; i++ {
		mm.queueMetrics(ActivityLogEntry{
			Timestamp: time.Now(),
			Model:     fmt.Sprintf("model-%d", i),
			ReqPath:   "/v1/chat/completions",
		})
	}

	count, err := store.Count()
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 3 {
		t.Fatalf("Count = %d, want 3", count)
	}

	entries, err := store.Query(0, 10)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("len(entries) = %d, want 3", len(entries))
	}
	if entries[0].Model != "model-5" || entries[2].Model != "model-3" {
		t.Fatalf("remaining entries = %+v, want newest three", entries)
	}
}

func TestServer_ParseMetrics_ChatCompletions(t *testing.T) {
	body := `{"usage":{"prompt_tokens":12,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":4}}}`
	parsed := gjson.Parse(body)
	entry, err := parseMetrics("m", time.Now(), parsed.Get("usage"), parsed.Get("timings"))
	if err != nil {
		t.Fatalf("parseMetrics: %v", err)
	}
	if entry.Tokens.InputTokens != 12 || entry.Tokens.OutputTokens != 7 || entry.Tokens.CachedTokens != 4 {
		t.Fatalf("tokens = %+v", entry.Tokens)
	}
}

func TestServer_ParseMetrics_Timings(t *testing.T) {
	body := `{"timings":{"prompt_n":20,"predicted_n":50,"prompt_per_second":100.0,"predicted_per_second":40.0,"prompt_ms":200,"predicted_ms":1250,"cache_n":8}}`
	parsed := gjson.Parse(body)
	entry, err := parseMetrics("m", time.Now(), parsed.Get("usage"), parsed.Get("timings"))
	if err != nil {
		t.Fatalf("parseMetrics: %v", err)
	}
	if entry.Tokens.InputTokens != 20 || entry.Tokens.OutputTokens != 50 || entry.Tokens.CachedTokens != 8 {
		t.Fatalf("tokens = %+v", entry.Tokens)
	}
	if entry.Tokens.TokensPerSecond != 40.0 || entry.Tokens.PromptPerSecond != 100.0 {
		t.Fatalf("rates = %+v", entry.Tokens)
	}
	if entry.DurationMs != 1450 {
		t.Fatalf("DurationMs = %d, want 1450", entry.DurationMs)
	}
}

func TestServer_ProcessStreamingResponse(t *testing.T) {
	body := []byte("data: {\"choices\":[{}]}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":15,\"completion_tokens\":33}}\n\n" +
		"data: [DONE]\n\n")
	entry, err := processStreamingResponse("m", time.Now(), body)
	if err != nil {
		t.Fatalf("processStreamingResponse: %v", err)
	}
	if entry.Tokens.InputTokens != 15 || entry.Tokens.OutputTokens != 33 {
		t.Fatalf("tokens = %+v", entry.Tokens)
	}
}

func TestServer_ProcessStreamingResponse_NoData(t *testing.T) {
	if _, err := processStreamingResponse("m", time.Now(), []byte("data: [DONE]\n\n")); err == nil {
		t.Fatal("expected error for stream with no usage data")
	}
}

func TestServer_ParseMetrics_Infill(t *testing.T) {
	// /infill responses are arrays; timings live in the last element.
	body := `[{"content":"a"},{"content":"b","timings":{"prompt_n":5,"predicted_n":9,"prompt_ms":10,"predicted_ms":20}}]`
	parsed := gjson.Parse(body)
	timings := parsed.Get("timings")
	if arr := parsed.Array(); len(arr) > 0 {
		timings = arr[len(arr)-1].Get("timings")
	}
	entry, err := parseMetrics("m", time.Now(), parsed.Get("usage"), timings)
	if err != nil {
		t.Fatalf("parseMetrics: %v", err)
	}
	if entry.Tokens.InputTokens != 5 || entry.Tokens.OutputTokens != 9 {
		t.Fatalf("tokens = %+v", entry.Tokens)
	}
}
