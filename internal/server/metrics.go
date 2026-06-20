package server

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mostlygeek/llama-swap/internal/activitylog"
	"github.com/mostlygeek/llama-swap/internal/cache"
	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/ring"
	"github.com/mostlygeek/llama-swap/internal/shared"
	"github.com/tidwall/gjson"
)

// TokenMetrics holds token usage and performance metrics.
type TokenMetrics struct {
	CachedTokens    int     `json:"cache_tokens"`
	InputTokens     int     `json:"input_tokens"`
	OutputTokens    int     `json:"output_tokens"`
	PromptPerSecond float64 `json:"prompt_per_second"`
	TokensPerSecond float64 `json:"tokens_per_second"`
	SpeedApprox     bool    `json:"speed_approx"`
}

// ActivityLogEntry represents parsed token statistics from llama-server logs.
type ActivityLogEntry struct {
	ID              int          `json:"id"`
	Timestamp       time.Time    `json:"timestamp"`
	Model           string       `json:"model"`
	ReqPath         string       `json:"req_path"`
	RespContentType string       `json:"resp_content_type"`
	RespStatusCode  int          `json:"resp_status_code"`
	Tokens          TokenMetrics `json:"tokens"`
	DurationMs      int          `json:"duration_ms"`
	HasCapture      bool         `json:"has_capture"`
}

// ActivityLogEvent carries a single activity log entry to event subscribers.
type ActivityLogEvent struct {
	Metrics ActivityLogEntry
}

func (e ActivityLogEvent) Type() uint32 {
	return shared.ActivityLogEventID
}

// MetricsResponse is the paginated response for GET /api/metrics.
type MetricsResponse struct {
	Entries []ActivityLogEntry `json:"entries"`
	Total   int                `json:"total"`
	HasMore bool               `json:"hasMore"`
}

// MetricsClearResponse is the response for POST /api/metrics/clear.
type MetricsClearResponse struct {
	Deleted int `json:"deleted"`
}

const metricsStorePruneInterval = 100

// metricsMonitor parses upstream responses for token statistics, keeps a
// bounded in-memory ring of recent activity, and (when captures are enabled)
// stores zstd+CBOR-compressed request/response captures in a sized cache.
// When a persistent store is configured, entries are also written to SQLite.
type metricsMonitor struct {
	mu      sync.RWMutex
	metrics ring.Buffer[ActivityLogEntry]
	nextID  int
	logger  *logmon.Monitor
	store   *activitylog.Store // nil = no persistence

	maxStoreMetrics       int
	storeWritesSincePrune int

	enableCaptures bool
	captureCache   *cache.Cache // zstd-compressed CBOR of ReqRespCapture
}

// newMetricsMonitor creates a metricsMonitor retaining up to maxMetrics entries.
// captureBufferMB is the capture buffer size in megabytes; 0 disables captures.
// store is the optional persistent backend; nil disables persistence.
// maxStoreMetrics limits persisted entries when store is configured; 0 disables pruning.
func newMetricsMonitor(logger *logmon.Monitor, maxMetrics int, captureBufferMB int, store *activitylog.Store, maxStoreMetrics int) *metricsMonitor {
	if maxMetrics <= 0 {
		maxMetrics = 1000
	}
	if maxStoreMetrics < 0 {
		maxStoreMetrics = 0
	}
	mm := &metricsMonitor{
		logger:          logger,
		metrics:         ring.NewBuffer[ActivityLogEntry](maxMetrics),
		store:           store,
		maxStoreMetrics: maxStoreMetrics,
		enableCaptures:  captureBufferMB > 0,
	}
	if captureBufferMB > 0 {
		mm.captureCache = cache.New(captureBufferMB * 1024 * 1024)
	}
	if store != nil && maxStoreMetrics > 0 {
		if _, err := store.DeleteOldest(maxStoreMetrics); err != nil {
			logger.Warnf("metrics store initial prune failed: %v", err)
		}
	}
	return mm
}

// queueMetrics adds a metric to the ring and optionally the persistent store.
// Returns the assigned ID.
func (mp *metricsMonitor) queueMetrics(metric ActivityLogEntry) int {
	mp.mu.Lock()
	defer mp.mu.Unlock()

	id := mp.nextID
	mp.nextID++

	if mp.store != nil {
		ae := activitylog.Entry{
			Timestamp:       metric.Timestamp.Format(time.RFC3339),
			Model:           metric.Model,
			ReqPath:         metric.ReqPath,
			RespContentType: metric.RespContentType,
			RespStatusCode:  metric.RespStatusCode,
			CachedTokens:    metric.Tokens.CachedTokens,
			InputTokens:     metric.Tokens.InputTokens,
			OutputTokens:    metric.Tokens.OutputTokens,
			PromptPerSecond: metric.Tokens.PromptPerSecond,
			TokensPerSecond: metric.Tokens.TokensPerSecond,
			DurationMs:      metric.DurationMs,
		}
		if dbID, err := mp.store.Insert(&ae); err == nil {
			id = dbID
			mp.pruneStoreIfNeeded()
		}
	}

	metric.ID = id
	mp.metrics.Push(metric)
	return metric.ID
}

func (mp *metricsMonitor) pruneStoreIfNeeded() {
	if mp.store == nil || mp.maxStoreMetrics <= 0 {
		return
	}

	mp.storeWritesSincePrune++
	pruneInterval := metricsStorePruneInterval
	if mp.maxStoreMetrics < pruneInterval {
		pruneInterval = mp.maxStoreMetrics
	}
	if mp.storeWritesSincePrune < pruneInterval {
		return
	}
	mp.storeWritesSincePrune = 0

	deleted, err := mp.store.DeleteOldest(mp.maxStoreMetrics)
	if err != nil {
		mp.logger.Warnf("metrics store prune failed: %v", err)
		return
	}
	if deleted > 0 {
		mp.retainCaptureCacheAfterClear(mp.maxStoreMetrics)
	}
}

// emitMetric publishes an ActivityLogEvent for the given metric.
func (mp *metricsMonitor) emitMetric(metric ActivityLogEntry) {
	event.Emit(ActivityLogEvent{Metrics: metric})
}

// getMetrics returns a copy of the current metrics (from store or ring buffer).
func (mp *metricsMonitor) getMetrics() []ActivityLogEntry {
	return mp.getMetricsPaginated(0, mp.getMetricsCount())
}

// getMetricsPaginated returns paginated metrics, newest first.
func (mp *metricsMonitor) getMetricsPaginated(offset, limit int) []ActivityLogEntry {
	mp.mu.RLock()
	defer mp.mu.RUnlock()

	var result []ActivityLogEntry

	if mp.store != nil {
		entries, err := mp.store.Query(offset, limit)
		if err != nil {
			mp.logger.Warnf("metrics store query failed: %v, falling back to ring buffer", err)
			return mp.ringBufferSlice()
		}
		result = mp.storeEntriesToActivity(entries)
	} else {
		result = mp.ringBufferSlice()
	}

	// Mark has_capture for entries that have a capture in the in-memory cache.
	if mp.captureCache != nil {
		for i := range result {
			result[i].HasCapture = result[i].HasCapture || mp.captureCache.Has(result[i].ID)
		}
	}

	return result
}

// ringBufferSlice returns the ring buffer contents as a slice.
func (mp *metricsMonitor) ringBufferSlice() []ActivityLogEntry {
	result := mp.metrics.Slice()
	if result == nil {
		return []ActivityLogEntry{}
	}
	return result
}

// storeEntriesToActivity converts persistent store entries to ActivityLogEntry.
func (mp *metricsMonitor) storeEntriesToActivity(entries []activitylog.Entry) []ActivityLogEntry {
	result := make([]ActivityLogEntry, len(entries))
	for i, e := range entries {
		ts, err := time.Parse(time.RFC3339, e.Timestamp)
		if err != nil {
			mp.logger.Warnf("metrics: invalid timestamp %q for entry %d, using current time: %v", e.Timestamp, e.ID, err)
			ts = time.Now()
		}
		result[i] = ActivityLogEntry{
			ID:              e.ID,
			Timestamp:       ts,
			Model:           e.Model,
			ReqPath:         e.ReqPath,
			RespContentType: e.RespContentType,
			RespStatusCode:  e.RespStatusCode,
			Tokens: TokenMetrics{
				CachedTokens:    e.CachedTokens,
				InputTokens:     e.InputTokens,
				OutputTokens:    e.OutputTokens,
				PromptPerSecond: e.PromptPerSecond,
				TokensPerSecond: e.TokensPerSecond,
			},
			DurationMs: e.DurationMs,
		}
	}
	return result
}

// getMetricsCount returns the total number of stored metrics.
func (mp *metricsMonitor) getMetricsCount() int {
	mp.mu.RLock()
	defer mp.mu.RUnlock()

	if mp.store != nil {
		if count, err := mp.store.Count(); err == nil {
			return count
		}
	}
	return mp.metrics.Len()
}

// getMetricsJSON returns the current metrics as a JSON array (legacy, unpaginated).
func (mp *metricsMonitor) getMetricsJSON() ([]byte, error) {
	return json.Marshal(mp.getMetrics())
}

// getMetricsPaginatedJSON returns a paginated metrics response as JSON.
// Both entries and total count are fetched under a single lock to avoid
// a race where the count changes between the two calls.
func (mp *metricsMonitor) getMetricsPaginatedJSON(offset, limit int) ([]byte, error) {
	entries, total := mp.getMetricsPaginatedAndCount(offset, limit)
	resp := MetricsResponse{
		Entries: entries,
		Total:   total,
		HasMore: offset+len(entries) < total,
	}
	return json.Marshal(resp)
}

// getSnapshotJSON returns the first page of metrics as a paginated JSON response.
// Used for SSE notifications after a clear so subscribers see the updated state.
func (mp *metricsMonitor) getSnapshotJSON() ([]byte, error) {
	return mp.getMetricsPaginatedJSON(0, metricsPageSize)
}

// getMetricsPaginatedAndCount returns paginated entries and the total count
// under a single lock, avoiding a race between the two operations.
func (mp *metricsMonitor) getMetricsPaginatedAndCount(offset, limit int) (entries []ActivityLogEntry, total int) {
	mp.mu.RLock()
	defer mp.mu.RUnlock()

	if mp.store != nil {
		storeEntries, err := mp.store.Query(offset, limit)
		if err != nil {
			mp.logger.Warnf("metrics store query failed: %v, falling back to ring buffer", err)
			return mp.ringBufferSlice(), mp.metrics.Len()
		}
		var terr error
		total, terr = mp.store.Count()
		if terr != nil {
			mp.logger.Warnf("metrics store count failed: %v, falling back to ring buffer count", terr)
			total = mp.metrics.Len()
		}
		entries = mp.storeEntriesToActivity(storeEntries)
	} else {
		entries = mp.ringBufferSlice()
		total = mp.metrics.Len()
	}

	// Mark has_capture for entries that have a capture in the in-memory cache.
	if mp.captureCache != nil {
		for i := range entries {
			entries[i].HasCapture = entries[i].HasCapture || mp.captureCache.Has(entries[i].ID)
		}
	}

	return entries, total
}

// clearMetrics removes metrics from the store and/or ring buffer.
// If keep > 0, only the most recent keep entries are retained.
// Returns the number of entries deleted.
func (mp *metricsMonitor) clearMetrics(keep int) int {
	mp.mu.Lock()
	defer mp.mu.Unlock()

	deleted := 0

	if mp.store != nil {
		if keep > 0 {
			n, err := mp.store.DeleteOldest(keep)
			if err != nil {
				mp.logger.Warnf("metrics store delete oldest failed: %v", err)
				return 0
			}
			deleted = n
		} else {
			n, err := mp.store.DeleteAll()
			if err != nil {
				mp.logger.Warnf("metrics store delete all failed: %v", err)
				return 0
			}
			deleted = n
		}
		// Only clear the in-memory ring buffer after confirming the store
		// operation succeeded, to avoid losing entries on transient errors.
		mp.metrics = ring.NewBuffer[ActivityLogEntry](mp.metrics.Cap())
		mp.retainCaptureCacheAfterClear(keep)
		return deleted
	}

	// No persistent store — clear only the in-memory ring buffer.
	cleared := mp.metrics.Len()
	mp.metrics = ring.NewBuffer[ActivityLogEntry](mp.metrics.Cap())
	mp.retainCaptureCacheAfterClear(0)
	return cleared
}

// retainCaptureCacheAfterClear removes captures for entries that were deleted
// from the activity log so cleared history cannot still be fetched by ID.
func (mp *metricsMonitor) retainCaptureCacheAfterClear(keep int) {
	if mp.captureCache == nil {
		return
	}
	if keep <= 0 || mp.store == nil {
		mp.captureCache.Clear()
		return
	}

	entries, err := mp.store.Query(0, keep)
	if err != nil {
		mp.logger.Warnf("metrics store query after clear failed: %v, clearing capture cache", err)
		mp.captureCache.Clear()
		return
	}

	ids := make(map[int]struct{}, len(entries))
	for _, entry := range entries {
		ids[entry.ID] = struct{}{}
	}
	mp.captureCache.Retain(ids)
}

// Close releases resources held by the metrics monitor.
func (mp *metricsMonitor) Close() {
	if mp.store != nil {
		if err := mp.store.Close(); err != nil {
			mp.logger.Warnf("metrics store close error: %v", err)
		}
	}
}

// record parses a completed response body and stores/emits an activity entry.
// When captures are enabled, a zstd+CBOR capture is stored for successful
// requests, with cf controlling which request/response parts are retained.
// reqBody and reqHeaders are the request data buffered before dispatch.
func (mp *metricsMonitor) record(modelID string, r *http.Request, recorder *responseBodyCopier, cf captureFields, reqBody []byte, reqHeaders map[string]string) {
	tm := ActivityLogEntry{
		Timestamp:       time.Now(),
		Model:           modelID,
		ReqPath:         r.URL.Path,
		RespContentType: recorder.Header().Get("Content-Type"),
		RespStatusCode:  recorder.Status(),
		DurationMs:      int(time.Since(recorder.StartTime()).Milliseconds()),
	}

	queueAndEmit := func() {
		tm.ID = mp.queueMetrics(tm)
		mp.emitMetric(tm)
	}

	if recorder.Status() != http.StatusOK {
		mp.logger.Warnf("non-200 response, recording partial metrics: status=%d, path=%s", recorder.Status(), r.URL.Path)
		queueAndEmit()
		return
	}

	body := recorder.body.Bytes()
	if len(body) == 0 {
		mp.logger.Warn("metrics: empty body, recording minimal metrics")
		queueAndEmit()
		return
	}

	if encoding := recorder.Header().Get("Content-Encoding"); encoding != "" {
		decoded, err := decompressBody(body, encoding)
		if err != nil {
			mp.logger.Warnf("metrics: decompression failed: %v, path=%s, recording minimal metrics", err, r.URL.Path)
			queueAndEmit()
			return
		}
		body = decoded
	}

	if strings.Contains(recorder.Header().Get("Content-Type"), "text/event-stream") {
		if parsed, err := processStreamingResponse(modelID, recorder.StartTime(), body); err != nil {
			mp.logger.Warnf("error processing streaming response: %v, path=%s, recording minimal metrics", err, r.URL.Path)
		} else {
			tm.Tokens = parsed.Tokens
			tm.DurationMs = parsed.DurationMs
		}
	} else if gjson.ValidBytes(body) {
		parsed := gjson.ParseBytes(body)
		usage := parsed.Get("usage")
		timings := parsed.Get("timings")

		// /infill responses are arrays; timings live in the last element (#463).
		if strings.HasPrefix(r.URL.Path, "/infill") {
			if arr := parsed.Array(); len(arr) > 0 {
				timings = arr[len(arr)-1].Get("timings")
			}
		}

		if usage.Exists() || timings.Exists() {
			if parsedMetrics, err := parseMetrics(modelID, recorder.StartTime(), usage, timings); err != nil {
				mp.logger.Warnf("error parsing metrics: %v, path=%s, recording minimal metrics", err, r.URL.Path)
			} else {
				tm.Tokens = parsedMetrics.Tokens
				tm.DurationMs = parsedMetrics.DurationMs
			}
		}
	} else {
		mp.logger.Warnf("metrics: invalid JSON in response body path=%s, recording minimal metrics", r.URL.Path)
	}

	tm.ID = mp.queueMetrics(tm)
	if mp.enableCaptures {
		capture := ReqRespCapture{
			ID:         tm.ID,
			ReqPath:    r.URL.Path,
			ReqHeaders: reqHeaders,
		}
		if cf&captureReqBody != 0 {
			capture.ReqBody = reqBody
		}
		if cf&captureRespHeaders != 0 {
			capture.RespHeaders = headerMap(recorder.Header())
			redactHeaders(capture.RespHeaders)
			delete(capture.RespHeaders, "Content-Encoding")
		}
		if cf&captureRespBody != 0 {
			capture.RespBody = body
		}
		if mp.addCapture(capture) {
			tm.HasCapture = true
		}
	}
	mp.emitMetric(tm)
}

// usagePaths lists the JSON paths where a per-event usage object can live.
var usagePaths = []string{"usage", "response.usage", "message.usage"}

// extractUsageTokens reads input/output/cached token counts from a usage
// gjson.Result, handling the field-name differences across endpoints.
func extractUsageTokens(usage gjson.Result) (input, output, cached int64, ok bool) {
	cached = -1
	if !usage.Exists() {
		return
	}

	if v := usage.Get("prompt_tokens"); v.Exists() {
		input = v.Int()
		ok = true
	} else if v := usage.Get("input_tokens"); v.Exists() {
		input = v.Int()
		ok = true
	}

	if v := usage.Get("completion_tokens"); v.Exists() {
		output = v.Int()
		ok = true
	} else if v := usage.Get("output_tokens"); v.Exists() {
		output = v.Int()
		ok = true
	}

	if v := usage.Get("cache_read_input_tokens"); v.Exists() {
		cached = v.Int()
		ok = true
	} else if v := usage.Get("input_tokens_details.cached_tokens"); v.Exists() {
		cached = v.Int()
		ok = true
	} else if v := usage.Get("prompt_tokens_details.cached_tokens"); v.Exists() {
		cached = v.Int()
		ok = true
	}
	return
}

func processStreamingResponse(modelID string, start time.Time, body []byte) (ActivityLogEntry, error) {
	var (
		inputTokens, outputTokens int64
		cachedTokens              int64 = -1
		hasAny                    bool
		timings                   gjson.Result
	)

	prefix := []byte("data:")
	for offset := 0; offset < len(body); {
		nl := bytes.IndexByte(body[offset:], '\n')
		var line []byte
		if nl == -1 {
			line = body[offset:]
			offset = len(body)
		} else {
			line = body[offset : offset+nl]
			offset += nl + 1
		}

		line = bytes.TrimSpace(line)
		if len(line) == 0 || !bytes.HasPrefix(line, prefix) {
			continue
		}
		data := bytes.TrimSpace(line[len(prefix):])
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		if !gjson.ValidBytes(data) {
			continue
		}
		parsed := gjson.ParseBytes(data)

		for _, path := range usagePaths {
			u := parsed.Get(path)
			if !u.Exists() {
				continue
			}
			i, o, c, ok := extractUsageTokens(u)
			if !ok {
				continue
			}
			hasAny = true
			if i > 0 {
				inputTokens = i
			}
			if o > 0 {
				outputTokens = o
			}
			if c >= 0 {
				cachedTokens = c
			}
		}
		if t := parsed.Get("timings"); t.Exists() {
			timings = t
			hasAny = true
		}
	}

	if !hasAny {
		return ActivityLogEntry{}, fmt.Errorf("no valid JSON data found in stream")
	}

	return buildMetrics(modelID, start, inputTokens, outputTokens, cachedTokens, timings), nil
}

func parseMetrics(modelID string, start time.Time, usage, timings gjson.Result) (ActivityLogEntry, error) {
	input, output, cached, _ := extractUsageTokens(usage)
	return buildMetrics(modelID, start, input, output, cached, timings), nil
}

// buildMetrics composes an ActivityLogEntry from accumulated token counts and
// optional llama-server timings (which override input/output and provide rates).
// When timings are absent, speed is approximated from wall-clock duration.
func buildMetrics(modelID string, start time.Time, inputTokens, outputTokens, cachedTokens int64, timings gjson.Result) ActivityLogEntry {
	wallDurationMs := int(time.Since(start).Milliseconds())
	durationMs := wallDurationMs
	tokensPerSecond := -1.0
	promptPerSecond := -1.0
	speedApprox := false

	if timings.Exists() {
		inputTokens = timings.Get("prompt_n").Int()
		outputTokens = timings.Get("predicted_n").Int()
		promptPerSecond = timings.Get("prompt_per_second").Float()
		tokensPerSecond = timings.Get("predicted_per_second").Float()
		timingsDurationMs := int(timings.Get("prompt_ms").Float() + timings.Get("predicted_ms").Float())
		if timingsDurationMs > durationMs {
			durationMs = timingsDurationMs
		}
		if cachedValue := timings.Get("cache_n"); cachedValue.Exists() {
			cachedTokens = cachedValue.Int()
		}
	} else {
		// Fallback: approximate speed from wall-clock duration.
		// This includes network overhead but provides useful data for backends
		// like vLLM that don't expose timing in API responses.
		if wallDurationMs > 0 {
			promptPerSecond = float64(inputTokens) / float64(wallDurationMs) * 1000.0
			tokensPerSecond = float64(outputTokens) / float64(wallDurationMs) * 1000.0
			speedApprox = true
		}
	}

	return ActivityLogEntry{
		Timestamp: time.Now(),
		Model:     modelID,
		Tokens: TokenMetrics{
			CachedTokens:    int(cachedTokens),
			InputTokens:     int(inputTokens),
			OutputTokens:    int(outputTokens),
			PromptPerSecond: promptPerSecond,
			TokensPerSecond: tokensPerSecond,
			SpeedApprox:     speedApprox,
		},
		DurationMs: durationMs,
	}
}

// decompressBody decompresses the body based on the Content-Encoding header.
func decompressBody(body []byte, encoding string) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "gzip":
		reader, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		defer reader.Close()
		return io.ReadAll(reader)
	case "deflate":
		reader := flate.NewReader(bytes.NewReader(body))
		defer reader.Close()
		return io.ReadAll(reader)
	default:
		return body, nil
	}
}

// filterAcceptEncoding filters Accept-Encoding to only gzip/deflate so response
// bodies remain decompressible for metrics parsing.
func filterAcceptEncoding(acceptEncoding string) string {
	if acceptEncoding == "" {
		return ""
	}

	supported := map[string]bool{"gzip": true, "deflate": true}
	var filtered []string
	for part := range strings.SplitSeq(acceptEncoding, ",") {
		encoding, _, _ := strings.Cut(strings.TrimSpace(part), ";")
		if supported[strings.ToLower(encoding)] {
			filtered = append(filtered, strings.TrimSpace(part))
		}
	}
	return strings.Join(filtered, ", ")
}

// responseBodyCopier tees the upstream response to the client while buffering
// it for metrics parsing. Status defaults to 200 until WriteHeader is called.
type responseBodyCopier struct {
	http.ResponseWriter
	body        *bytes.Buffer
	tee         io.Writer
	status      int
	wroteHeader bool
	start       time.Time
}

func newBodyCopier(w http.ResponseWriter) *responseBodyCopier {
	buf := &bytes.Buffer{}
	return &responseBodyCopier{
		ResponseWriter: w,
		body:           buf,
		tee:            io.MultiWriter(w, buf),
		status:         http.StatusOK,
		start:          time.Now(),
	}
}

func (w *responseBodyCopier) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	// On a protocol upgrade (e.g. websocket) the body is raw framed data, not a
	// metrics-parseable response, so write straight to the client without
	// buffering a copy we can't use.
	if w.status == http.StatusSwitchingProtocols {
		return w.ResponseWriter.Write(b)
	}
	return w.tee.Write(b)
}

func (w *responseBodyCopier) WriteHeader(statusCode int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}

// Flush forwards to the underlying writer so streaming responses still flush.
func (w *responseBodyCopier) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack forwards to the underlying writer so httputil.ReverseProxy can take
// over the connection for websocket upgrades.
func (w *responseBodyCopier) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, fmt.Errorf("underlying ResponseWriter does not support hijacking")
}

func (w *responseBodyCopier) Status() int          { return w.status }
func (w *responseBodyCopier) StartTime() time.Time { return w.start }
