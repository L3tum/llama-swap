package activitylog

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tempDB creates a temporary database file and returns its path.
// The caller is responsible for cleaning up the file.
func tempDB(t *testing.T) string {
	t.Helper()
	f, err := os.CreateTemp("", "activitylog-test-*.db")
	require.NoError(t, err)
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })
	return f.Name()
}

func makeEntry(id int64, model, path string) Entry {
	return Entry{
		ID:              id,
		Timestamp:       "2025-01-15T10:00:00Z",
		Model:           model,
		ReqPath:         path,
		RespContentType: "application/json",
		RespStatusCode:  200,
		CachedTokens:    10,
		InputTokens:     50,
		OutputTokens:    100,
		PromptPerSecond: 100.5,
		TokensPerSecond: 200.3,
		DurationMs:      500,
		HasCapture:      false,
	}
}

func TestNew(t *testing.T) {
	// Empty path returns nil
	store, err := New("")
	assert.NoError(t, err)
	assert.Nil(t, store)

	// Valid path creates a store
	path := tempDB(t)
	store, err = New(path)
	require.NoError(t, err)
	require.NotNil(t, store)
	require.NoError(t, store.Close())
}

func TestInsertAndQuery(t *testing.T) {
	path := tempDB(t)
	store, err := New(path)
	require.NoError(t, err)
	defer store.Close()

	// Insert a single entry
	e := makeEntry(0, "test-model", "/v1/chat/completions")
	id, err := store.Insert(&e)
	require.NoError(t, err)
	assert.Greater(t, id, int64(0))

	// Query it back
	entries, err := store.Query(0, 10)
	require.NoError(t, err)
	assert.Len(t, entries, 1)
	assert.Equal(t, "test-model", entries[0].Model)
	assert.Equal(t, "/v1/chat/completions", entries[0].ReqPath)
}

func TestQueryOrdering(t *testing.T) {
	path := tempDB(t)
	store, err := New(path)
	require.NoError(t, err)
	defer store.Close()

	// Insert entries with different models
	for i := 0; i < 5; i++ {
		e := makeEntry(int64(i), fmt.Sprintf("model-%d", i), fmt.Sprintf("/path-%d", i))
		_, err := store.Insert(&e)
		require.NoError(t, err)
	}

	// Query should return newest first (highest ID first)
	entries, err := store.Query(0, 10)
	require.NoError(t, err)
	assert.Len(t, entries, 5)
	assert.Equal(t, "model-4", entries[0].Model) // newest
	assert.Equal(t, "model-0", entries[4].Model) // oldest
}

func TestQueryPagination(t *testing.T) {
	path := tempDB(t)
	store, err := New(path)
	require.NoError(t, err)
	defer store.Close()

	// Insert 10 entries
	for i := 0; i < 10; i++ {
		e := makeEntry(int64(i), fmt.Sprintf("model-%d", i), "/path")
		_, err := store.Insert(&e)
		require.NoError(t, err)
	}

	// Page 1: first 3
	page1, err := store.Query(0, 3)
	require.NoError(t, err)
	assert.Len(t, page1, 3)
	assert.Equal(t, "model-9", page1[0].Model) // newest
	assert.Equal(t, "model-7", page1[2].Model)

	// Page 2: next 3
	page2, err := store.Query(3, 3)
	require.NoError(t, err)
	assert.Len(t, page2, 3)
	assert.Equal(t, "model-6", page2[0].Model)
	assert.Equal(t, "model-4", page2[2].Model)

	// Page 3: next 3
	page3, err := store.Query(6, 3)
	require.NoError(t, err)
	assert.Len(t, page3, 3)
	assert.Equal(t, "model-3", page3[0].Model)

	// Page 4: last 1
	page4, err := store.Query(9, 3)
	require.NoError(t, err)
	assert.Len(t, page4, 1)
	assert.Equal(t, "model-0", page4[0].Model)

	// Page 5: beyond the end
	page5, err := store.Query(10, 3)
	require.NoError(t, err)
	assert.Empty(t, page5)
}

func TestCount(t *testing.T) {
	path := tempDB(t)
	store, err := New(path)
	require.NoError(t, err)
	defer store.Close()

	count, err := store.Count()
	require.NoError(t, err)
	assert.Equal(t, 0, count)

	for i := 0; i < 5; i++ {
		e := makeEntry(int64(i), "model", "/path")
		_, err := store.Insert(&e)
		require.NoError(t, err)
	}

	count, err = store.Count()
	require.NoError(t, err)
	assert.Equal(t, 5, count)
}

func TestDeleteAll(t *testing.T) {
	path := tempDB(t)
	store, err := New(path)
	require.NoError(t, err)
	defer store.Close()

	for i := 0; i < 5; i++ {
		e := makeEntry(int64(i), "model", "/path")
		_, err := store.Insert(&e)
		require.NoError(t, err)
	}

	deleted, err := store.DeleteAll()
	require.NoError(t, err)
	assert.Equal(t, 5, deleted)

	count, err := store.Count()
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

func TestDeleteOldest(t *testing.T) {
	path := tempDB(t)
	store, err := New(path)
	require.NoError(t, err)
	defer store.Close()

	// Insert 10 entries
	for i := 0; i < 10; i++ {
		e := makeEntry(int64(i), fmt.Sprintf("model-%d", i), "/path")
		_, err := store.Insert(&e)
		require.NoError(t, err)
	}

	// Keep only the 3 most recent
	deleted, err := store.DeleteOldest(3)
	require.NoError(t, err)
	assert.Equal(t, 7, deleted)

	count, err := store.Count()
	require.NoError(t, err)
	assert.Equal(t, 3, count)

	// Verify the remaining entries are the newest ones
	entries, err := store.Query(0, 10)
	require.NoError(t, err)
	assert.Len(t, entries, 3)
	assert.Equal(t, "model-9", entries[0].Model)
	assert.Equal(t, "model-8", entries[1].Model)
	assert.Equal(t, "model-7", entries[2].Model)
}

func TestDeleteOldestKeepZero(t *testing.T) {
	path := tempDB(t)
	store, err := New(path)
	require.NoError(t, err)
	defer store.Close()

	for i := 0; i < 5; i++ {
		e := makeEntry(int64(i), "model", "/path")
		_, err := store.Insert(&e)
		require.NoError(t, err)
	}

	deleted, err := store.DeleteOldest(0)
	require.NoError(t, err)
	assert.Equal(t, 5, deleted)

	count, err := store.Count()
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

func TestConcurrentInserts(t *testing.T) {
	path := tempDB(t)
	store, err := New(path)
	require.NoError(t, err)
	defer store.Close()

	const numGoroutines = 10
	const entriesPerGoroutine = 20

	done := make(chan bool, numGoroutines)
	for g := 0; g < numGoroutines; g++ {
		go func(gID int) {
			for i := 0; i < entriesPerGoroutine; i++ {
				e := makeEntry(int64(gID*entriesPerGoroutine+i), fmt.Sprintf("model-g%d-e%d", gID, i), "/path")
				_, err := store.Insert(&e)
				require.NoError(t, err)
			}
			done <- true
		}(g)
	}

	// Wait for all goroutines
	for i := 0; i < numGoroutines; i++ {
		<-done
	}

	count, err := store.Count()
	require.NoError(t, err)
	assert.Equal(t, numGoroutines*entriesPerGoroutine, count)
}

func TestEmptyQueryReturnsEmptySlice(t *testing.T) {
	path := tempDB(t)
	store, err := New(path)
	require.NoError(t, err)
	defer store.Close()

	entries, err := store.Query(0, 10)
	require.NoError(t, err)
	assert.NotNil(t, entries)
	assert.Empty(t, entries)
}
