// Package engine is the public facade wiring the page store, index, data
// blocks and query engine together behind an InfluxDB-flavoured API.
//
// Writes buffer into an in-memory memtable; Commit flushes each touched
// series to a new immutable data block, updates the index and durably
// persists both via the page store's write-ahead log. Concurrency: one
// RWMutex — writers and Commit take the write lock, readers proceed
// concurrently under the read lock.
package engine

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/Kelmory/local-tsdb/internal/block"
	"github.com/Kelmory/local-tsdb/internal/index"
	"github.com/Kelmory/local-tsdb/internal/model"
	"github.com/Kelmory/local-tsdb/internal/pagestore"
	"github.com/Kelmory/local-tsdb/internal/query"
)

// blockChunkSize caps the rows per committed data block so range queries
// only decode the chunks overlapping the requested time window.
const blockChunkSize = 4096

// Engine owns the database file and all in-memory state.
type Engine struct {
	mu       sync.RWMutex
	store    *pagestore.PageStore
	index    *index.Index
	memtable map[string][]model.Point
	cache    sync.Map // locator (int64) -> *block.Block (blocks are immutable)
}

// Open creates or opens a single-file database (plus path+".wal").
func Open(path string) (*Engine, error) {
	store, err := pagestore.Open(path)
	if err != nil {
		return nil, err
	}
	var ix *index.Index
	if store.Header.RootIndexPage != pagestore.FreeNone {
		blob, err := store.ReadBlob(int(store.Header.RootIndexPage))
		if err != nil {
			store.Close(false)
			return nil, fmt.Errorf("reading index: %w", err)
		}
		ix, err = index.FromBytes(blob)
		if err != nil {
			store.Close(false)
			return nil, fmt.Errorf("parsing index: %w", err)
		}
	} else {
		ix = index.New()
	}
	return &Engine{
		store:    store,
		index:    ix,
		memtable: map[string][]model.Point{},
	}, nil
}

// readBlock decodes a data block by locator, caching decoded results.
// Data blocks are immutable and their locators are never reused, so cache
// entries never invalidate.
func (e *Engine) readBlock(locator int) (*block.Block, error) {
	if v, ok := e.cache.Load(int64(locator)); ok {
		return v.(*block.Block), nil
	}
	blob, err := e.store.ReadBlob(locator)
	if err != nil {
		return nil, fmt.Errorf("reading block %d: %w", locator, err)
	}
	blk, err := block.FromBytes(blob)
	if err != nil {
		return nil, fmt.Errorf("decoding block %d: %w", locator, err)
	}
	e.cache.Store(int64(locator), blk)
	return blk, nil
}

// WritePoints buffers points for the next commit. A zero Timestamp is
// replaced with the current time (once per call, like the reference).
// Returns the number of points buffered.
func (e *Engine) WritePoints(pts []model.Point) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now().UnixNano()
	for _, p := range pts {
		ts := p.Timestamp
		if ts == 0 {
			ts = now
		}
		pt := model.Point{
			Measurement: p.Measurement,
			Tags:        p.Tags,
			Fields:      p.Fields,
			Timestamp:   ts,
		}
		key := e.index.AddSeries(pt.Measurement, pt.Tags)
		e.memtable[key] = append(e.memtable[key], pt)
	}
	return len(pts), nil
}

// Commit flushes the memtable: one immutable DataBlock per touched series,
// index update, and a WAL-durably persisted page transaction. All blocks are
// built and validated BEFORE any page allocation so a failed commit leaves
// the store untouched. No-op when the memtable is empty.
func (e *Engine) Commit() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.memtable) == 0 {
		return nil
	}
	store := e.store
	store.Begin()

	// Build and validate every block first: a field-type conflict must not
	// leave staged pages or a mutated header behind. Each series' points are
	// split into fixed-size chunks so time-range queries can prune whole
	// chunks instead of decoding one monolithic block per series.
	keys := make([]string, 0, len(e.memtable))
	for key := range e.memtable {
		keys = append(keys, key)
	}
	blocks := make(map[string][]*block.Block, len(keys))
	for _, key := range keys {
		pts := e.memtable[key]
		var chunks []*block.Block
		for start := 0; start < len(pts); start += blockChunkSize {
			stop := start + blockChunkSize
			if stop > len(pts) {
				stop = len(pts)
			}
			blk, err := block.FromPoints(pts[start:stop])
			if err != nil {
				store.Begin() // discard any staged pages
				return err
			}
			chunks = append(chunks, blk)
		}
		blocks[key] = chunks
	}

	// Free the previous index blob *first* so its pages can be reused by the
	// allocations below.
	oldRoot := store.Header.RootIndexPage
	if oldRoot != pagestore.FreeNone {
		if err := store.FreeBlob(int(oldRoot)); err != nil {
			return err
		}
	}
	for _, key := range keys {
		for _, blk := range blocks[key] {
			locator, err := store.WriteBlob(blk.ToBytes())
			if err != nil {
				return err
			}
			minTs, _ := blk.MinTs()
			maxTs, _ := blk.MaxTs()
			if err := e.index.AddBlock(key, index.BlockRef{Locator: locator, MinTs: minTs, MaxTs: maxTs}); err != nil {
				return err
			}
		}
	}
	newRoot, err := store.WriteBlob(e.index.ToBytes())
	if err != nil {
		return err
	}
	store.Header.RootIndexPage = int64(newRoot)
	if err := store.Commit(); err != nil {
		return err
	}
	e.memtable = map[string][]model.Point{}
	return nil
}

// SelectQuery describes a point scan.
type SelectQuery struct {
	Measurement string            // "" = all measurements
	TagFilters  map[string]string // equality filters (intersected)
	Fields      []string          // nil = all fields
	Start, End  *int64            // nil = unbounded; inclusive
	Limit       int               // 0 = no limit
}

// Select returns matching points ordered by (series key, timestamp).
func (e *Engine) Select(ctx context.Context, q SelectQuery) ([]model.Point, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	qe := query.New(e.index, e.readBlock, e.memtable)
	return qe.Select(q.Measurement, q.TagFilters, q.Fields, q.Start, q.End, q.Limit)
}

// AggregateRequest describes a fold over matching rows.
type AggregateRequest = query.AggregateRequest

// AggregateResult is scalar (Groups == nil) or per-tag-value.
type AggregateResult = query.AggregateResult

// Aggregate folds matching rows into a scalar or per-tag-group result.
func (e *Engine) Aggregate(ctx context.Context, req AggregateRequest) (AggregateResult, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return AggregateResult{}, err
	}
	qe := query.New(e.index, e.readBlock, e.memtable)
	return qe.Aggregate(req)
}

// ExecQuery runs a query-engine level scan/fold; the InfluxQL layer calls it.
func (e *Engine) QueryEngine() *query.QueryEngine {
	return query.New(e.index, e.readBlock, e.memtable)
}

// RLock exposes the read lock to the SQL layer (which drives the query
// engine directly). Callers MUST call RUnlock.
func (e *Engine) RLock()   { e.mu.RLock() }
func (e *Engine) RUnlock() { e.mu.RUnlock() }

// Measurements returns every measurement name, sorted.
func (e *Engine) Measurements() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.index.Measurements()
}

// Series returns canonical keys: all of them when measurement is "", else
// those of exactly that measurement.
func (e *Engine) Series(measurement string) []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	keys := e.index.AllSeries()
	if measurement == "" {
		return keys
	}
	prefix := measurement + ","
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if k == measurement || len(k) > len(prefix) && k[:len(prefix)] == prefix {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Index exposes the index (for tests and advanced tooling).
func (e *Engine) Index() *index.Index { return e.index }

// Close checkpoints (removing the WAL) and closes the file.
func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.store.Close(true)
}
