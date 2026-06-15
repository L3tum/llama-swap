# Persist Activity Log to SQLite

## Overview

Add SQLite-backed persistence for the Activity tab so that request history survives server restarts and config reloads. Uses a pure-Go SQLite implementation (`modernc.org/sqlite`) to avoid CGO dependency and keep cross-compilation simple.

## Current Architecture

- `ActivityLogEntry` records are stored in `metricsMonitor.metrics`, a `ring.Buffer[ActivityLogEntry]` (in-memory circular buffer)
- Buffer size controlled by `cfg.MetricsMaxInMemory` (default 1000)
- Entries are pushed via `queueMetrics()` which assigns an auto-incrementing `ID` starting at 0
- Served via `GET /api/metrics` (JSON snapshot) and `GET /api/events` (SSE stream)
- Frontend clears metrics on SSE disconnect, receives full snapshot on reconnect
- **Problem**: all data is lost on restart or config reload (which creates a new `metricsMonitor`)

## Design

### Storage Layer

New package: `internal/activitylog/`

```go
package activitylog

// Store provides persistent storage for ActivityLogEntry records.
type Store struct {
    db   *sql.DB
    mu   sync.Mutex
    path string
}

// New opens (or creates) the SQLite database and returns a Store.
// Returns nil if dbPath is empty (no persistence).
func New(dbPath string) (*Store, error)

// Insert adds a single entry. Returns the assigned ID.
func (s *Store) Insert(entry *ActivityLogEntry) (int, error)

// Query returns paginated entries, ordered newest-first.
// offset is the number of entries to skip, limit is the max to return.
func (s *Store) Query(offset, limit int) ([]ActivityLogEntry, error)

// Count returns the total number of stored entries.
func (s *Store) Count() (int, error)

// DeleteAll removes all entries from the database.
func (s *Store) DeleteAll() error

// Close closes the database connection.
func (s *Store) Close() error
```

### SQLite Schema

Single table, WAL mode for concurrent read/write:

```sql
CREATE TABLE IF NOT EXISTS activity (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp     TEXT    NOT NULL,  -- RFC3339
    model         TEXT    NOT NULL,
    req_path      TEXT    NOT NULL,
    resp_content_type TEXT NOT NULL,
    resp_status_code INTEGER NOT NULL DEFAULT 0,
    cached_tokens INTEGER NOT NULL DEFAULT 0,
    input_tokens  INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    prompt_per_second REAL NOT NULL DEFAULT -1,
    tokens_per_second REAL NOT NULL DEFAULT -1,
    duration_ms   INTEGER NOT NULL DEFAULT 0,
    has_capture   INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_activity_timestamp ON activity(timestamp DESC);
```

**Rationale for flat schema:** Denormalizing `TokenMetrics` into columns avoids JOINs on every query. The table is append-only and read as a whole, so normalization provides no benefit.

### ID Strategy

Current IDs are auto-incrementing integers starting at 0, reset on restart. With persistence:
- SQLite's `AUTOINCREMENT` provides unique, monotonically increasing IDs
- The `ActivityLogEntry.ID` field is populated from the SQLite row ID
- Frontend displays `metric.id + 1` (current behavior), so IDs will just be larger numbers — no UI change needed
- Capture cache uses the same ID; captures remain in-memory-only and are lost on restart (unchanged)

### Integration with `metricsMonitor`

The `metricsMonitor` gains an optional `*activitylog.Store` backend:

```go
type metricsMonitor struct {
    mu      sync.RWMutex
    metrics ring.Buffer[ActivityLogEntry]  // in-memory ring (recent entries)
    store   *activitylog.Store             // persistent backend (nil = disabled)
    logger  *logmon.Monitor
    // ... existing fields
}
```

**Write path** (`queueMetrics`):
1. Assign temporary ID
2. If store is not nil, insert into SQLite and get the real ID
3. Push to ring buffer as before

**Read path** (`getMetrics`):
- **With store**: query the store with `offset`/`limit` parameters from the request
- **Without store**: fall back to ring buffer (current behavior)
- The ring buffer continues to serve as the in-memory cache for very recent entries.
  When the store is available, the first page (offset=0) is served from the store
  so that persisted history is visible immediately after restart.

**Rationale**: On startup, the ring buffer is empty. By querying the store, the UI
immediately shows the full history. As new entries arrive, they go into both the
store and the ring buffer. The ring buffer capacity still limits how many *recent*
entries are cached in memory, but the store provides the full history.

### Configuration

Add to `Config` struct in `internal/config/config.go`:

```go
type Config struct {
    // ... existing fields
    MetricsDBPath string `yaml:"metricsDBPath"` // SQLite DB path; empty = no persistence
}
```

- Default: `""` (disabled, current behavior)
- When set: path to a SQLite database file (e.g., `"llama-swap-metrics.db"`)
- No new data directory concept — user specifies the full path

Wire into `server.New()`:
```go
// In server.go New():
var store *activitylog.Store
if cfg.MetricsDBPath != "" {
    var err error
    store, err = activitylog.New(cfg.MetricsDBPath)
    if err != nil {
        proxylog.Warnf("failed to open metrics DB: %v, activity log will not persist", err)
        store = nil
    }
}
mm := newMetricsMonitor(proxylog, cfg.MetricsMaxInMemory, cfg.CaptureBuffer, store)
```

### Graceful Shutdown

Add a `Close()` method to `metricsMonitor` that calls `store.Close()`. Call this from `Server.Shutdown()`:

```go
func (s *Server) Shutdown(timeout time.Duration) error {
    // ... existing code
    if s.metrics != nil {
        s.metrics.Close()
    }
    // ...
}
```

## Dependency

Add `modernc.org/sqlite` (pure-Go SQLite, no CGO):

```
go get modernc.org/sqlite
```

This is the standard choice for Go projects that want SQLite without a C toolchain. It embeds the entire SQLite engine as Go code.

Alternative considered: `github.com/mattn/go-sqlite3` (CGO-based). Rejected because it requires a C compiler, complicating cross-compilation and the release build.

## Frontend Changes

### Pagination — Infinite Scroll

**API change — `GET /api/metrics`**

Add `offset` and `limit` query parameters:

```
GET /api/metrics?offset=0&limit=50
```

Response changes from `[]ActivityLogEntry` to:

```typescript
interface MetricsResponse {
    entries: ActivityLogEntry[];
    total: number;       // total rows in DB (or ring buffer size if no DB)
    hasMore: boolean;    // true if offset + len(entries) < total
}
```

- Default `offset=0`, `limit=50` when not specified
- When no store is configured, the endpoint still supports these params but
  reads from the ring buffer (total = ring buffer length)
- `hasMore` lets the frontend know whether to show a "load more" trigger

**SSE behaviour**: unchanged. SSE still pushes the first page on connect
(offset=0, limit=50). Real-time entries are pushed individually as before.

**Infinite scroll implementation** (in `Activity.svelte`):

1. Initial load: request `offset=0&limit=50` via SSE snapshot (newest first)
2. Add an `IntersectionObserver` sentinel element at the **bottom** of the list
   (scrolling down loads older entries)
3. When the sentinel becomes visible, fetch the next page via
   `GET /api/metrics?offset={currentOffset + pageSize}&limit=50`
4. Append the older entries to the end of the list
5. Show a loading spinner while fetching
6. When `hasMore` is false, hide the sentinel

This keeps the existing scroll direction: newest at the top, older entries
revealed as you scroll down.

### Clear Button

**New API endpoint — `POST /api/metrics/clear`**

```http
POST /api/metrics/clear
Content-Type: application/json

{ "keep": 500 }
```

- If `keep` is omitted or 0: delete **all** entries
- If `keep` > 0: delete all except the most recent `keep` entries
- Returns `{ "deleted": 1234 }` with the number of rows deleted
- If no store is configured: returns `{ "deleted": 0 }` and clears the
  in-memory ring buffer

**Frontend UI** (in `Activity.svelte`):

- Add a button in the tab header (next to the tab title or in the right side):
  `` `<button class="btn btn-sm btn-danger" onclick={confirmClear}>Clear History</button>` ``
- On click, show a browser `confirm()` dialog: "Clear all activity history?"
- On confirmation, call `POST /api/metrics/clear`
- On success, clear the frontend metrics store

### TypeScript Type Updates

In `ui-svelte/src/lib/types.ts`, add:

```typescript
export interface MetricsResponse {
    entries: ActivityLogEntry[];
    total: number;
    hasMore: boolean;
}

export interface MetricsClearResponse {
    deleted: number;
}
```

## Files to Modify

| File | Change |
|------|--------|
| `go.mod` / `go.sum` | Add `modernc.org/sqlite` dependency |
| `internal/config/config.go` | Add `MetricsDBPath` field to `Config`, update defaults |
| `internal/server/metrics.go` | Add `store` field, pagination support in `getMetrics`, `clearMetrics()`, `Close()` |
| `internal/server/apigroup.go` | Register `POST /api/metrics/clear` handler |
| `internal/server/server.go` | Open store in `New()`, close in `Shutdown()` |
| `internal/activitylog/store.go` | **NEW** — SQLite store implementation |
| `internal/activitylog/store_test.go` | **NEW** — Unit tests for store |
| `config.example.yaml` | Document `metricsDBPath` option |
| `config-schema.json` | Add `metricsDBPath` to JSON schema |
| `ui-svelte/src/lib/types.ts` | Add `MetricsResponse` and `MetricsClearResponse` types |
| `ui-svelte/src/stores/api.ts` | Add pagination state, `clearMetrics()` call |
| `ui-svelte/src/routes/Activity.svelte` | Add infinite scroll + clear button |

## Testing Plan

1. **Unit tests** (`activitylog/store_test.go`):
   - Insert and query a single entry
   - Insert multiple entries, verify ordering (newest first)
   - Pagination: insert 100 entries, query with offset/limit, verify correct page
   - `DeleteAll` removes all entries
   - Empty database returns empty slice
   - Concurrent inserts don't corrupt data

2. **Integration tests** (add to `server/extras_test.go` or new file):
   - Create server with `MetricsDBPath` set
   - Record a metric, restart server (create new `metricsMonitor` with same DB path)
   - Verify metric is still queryable
   - Test `POST /api/metrics/clear` endpoint
   - Test `GET /api/metrics?offset=5&limit=10` returns correct page

3. **Existing tests**: Run `make test-dev` to ensure no regressions

## Checklist

- [ ] Add `modernc.org/sqlite` dependency via `go get`
- [ ] Create `internal/activitylog/store.go` with SQLite schema and CRUD
- [ ] Create `internal/activitylog/store_test.go` with unit tests
- [ ] Add `MetricsDBPath` to `Config` struct
- [ ] Update `config-schema.json` with new field
- [ ] Update `config.example.yaml` with documentation
- [ ] Modify `metricsMonitor` to accept and use `*activitylog.Store`
- [ ] Wire store creation into `server.New()`
- [ ] Add `metricsMonitor.Close()` and call from `Server.Shutdown()`
- [ ] Update `queueMetrics` to write to store
- [ ] Update `getMetrics` to support `offset`/`limit` query params and return `MetricsResponse`
- [ ] Add `clearMetrics()` method to `metricsMonitor` and wire `POST /api/metrics/clear`
- [ ] Add `MetricsResponse` and `MetricsClearResponse` TypeScript types
- [ ] Update frontend `api.ts` with pagination state and clear function
- [ ] Update `Activity.svelte` with infinite scroll sentinel and clear button
- [ ] Run `make test-dev` — fix any issues
- [ ] Run `make test-all` — final validation
- [ ] Manual test: start server, make requests, restart, verify Activity tab shows history
- [ ] Manual test: scroll up, verify older entries load
- [ ] Manual test: click clear button, verify history is cleared

## Risks & Mitigations

| Risk | Mitigation |
|------|-----------|
| SQLite file corruption | WAL mode + `PRAGMA journal_mode=WAL`; single-writer design minimizes risk |
| Database file grows unbounded | User-controlled via "Clear History" button in the Activity tab; SQLite handles millions of rows trivially for this workload |
| Pure-Go SQLite is slower than CGO | Activity log writes are infrequent (one per API call); query is single-table scan with LIMIT; acceptable |
| Binary size increase | `modernc.org/sqlite` adds ~5-10MB to binary; acceptable for a feature that many users will want |
| Config reload creates new store | Each reload opens a new store pointing to the same file; old store is closed; no data loss |
