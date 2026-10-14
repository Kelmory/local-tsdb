// Package index maintains the mapping from canonical series keys to their
// data blocks, plus derived measurement and tag-inverted lookup structures.
// Only the series_key -> [block refs] table is persisted; everything else is
// rebuilt from the parsed keys on load.
package index

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"sort"

	"github.com/Kelmory/local-tsdb/internal/model"
)

// BlockRef locates one data block: its blob start page and time bounds.
type BlockRef struct {
	Locator int
	MinTs   int64
	MaxTs   int64
}

type tagKey struct{ name, value string }

// Index is the in-memory series -> blocks map with derived lookups.
type Index struct {
	SeriesBlocks map[string][]BlockRef
	measurements map[string]bool
	tags         map[tagKey]map[string]bool
}

// New creates an empty index.
func New() *Index {
	return &Index{
		SeriesBlocks: map[string][]BlockRef{},
		measurements: map[string]bool{},
		tags:         map[tagKey]map[string]bool{},
	}
}

// AddSeries registers a series (no-op if known) and returns its canonical key.
func (ix *Index) AddSeries(measurement string, tags map[string]string) string {
	key := model.SeriesKey(measurement, tags)
	if _, ok := ix.SeriesBlocks[key]; !ok {
		ix.SeriesBlocks[key] = nil
		ix.measurements[measurement] = true
		for name, value := range tags {
			k := tagKey{name, value}
			set, ok := ix.tags[k]
			if !ok {
				set = map[string]bool{}
				ix.tags[k] = set
			}
			set[key] = true
		}
	}
	return key
}

// AddBlock appends a block reference to an existing series.
func (ix *Index) AddBlock(seriesKey string, ref BlockRef) error {
	if _, ok := ix.SeriesBlocks[seriesKey]; !ok {
		return fmt.Errorf("unknown series %q", seriesKey)
	}
	ix.SeriesBlocks[seriesKey] = append(ix.SeriesBlocks[seriesKey], ref)
	return nil
}

// Blocks returns the block refs of a series (nil for unknown series).
func (ix *Index) Blocks(seriesKey string) []BlockRef {
	return ix.SeriesBlocks[seriesKey]
}

// Measurements returns every measurement name, sorted.
func (ix *Index) Measurements() []string {
	out := make([]string, 0, len(ix.measurements))
	for m := range ix.measurements {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// AllSeries returns every canonical series key, sorted.
func (ix *Index) AllSeries() []string {
	out := make([]string, 0, len(ix.SeriesBlocks))
	for k := range ix.SeriesBlocks {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// SeriesForTags returns the series keys matching *every* supplied
// tag_name=tag_value pair (all keys when no filters are given).
func (ix *Index) SeriesForTags(filters map[string]string) map[string]bool {
	if len(filters) == 0 {
		out := make(map[string]bool, len(ix.SeriesBlocks))
		for k := range ix.SeriesBlocks {
			out[k] = true
		}
		return out
	}
	var result map[string]bool
	for name, value := range filters {
		s := ix.tags[tagKey{name, value}]
		if result == nil {
			result = make(map[string]bool, len(s))
			for k := range s {
				result[k] = true
			}
		} else {
			for k := range result {
				if !s[k] {
					delete(result, k)
				}
			}
		}
	}
	if result == nil {
		return map[string]bool{}
	}
	return result
}

// ToBytes serializes the persisted table (series key -> block refs) with a
// trailing CRC32.
func (ix *Index) ToBytes() []byte {
	keys := make([]string, 0, len(ix.SeriesBlocks))
	for k := range ix.SeriesBlocks {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]byte, 0, 64)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(keys)))
	for _, key := range keys {
		out = binary.LittleEndian.AppendUint16(out, uint16(len(key)))
		out = append(out, key...)
		refs := ix.SeriesBlocks[key]
		out = binary.LittleEndian.AppendUint32(out, uint32(len(refs)))
		for _, r := range refs {
			out = binary.LittleEndian.AppendUint64(out, uint64(r.Locator))
			out = binary.LittleEndian.AppendUint64(out, uint64(r.MinTs))
			out = binary.LittleEndian.AppendUint64(out, uint64(r.MaxTs))
		}
	}
	return binary.LittleEndian.AppendUint32(out, crc32.ChecksumIEEE(out))
}

// FromBytes restores an index, rebuilding the derived structures.
func FromBytes(data []byte) (*Index, error) {
	ix := New()
	if len(data) == 0 {
		return ix, nil
	}
	if len(data) < 4 {
		return nil, fmt.Errorf("index truncated")
	}
	body, sum := data[:len(data)-4], binary.LittleEndian.Uint32(data[len(data)-4:])
	if crc32.ChecksumIEEE(body) != sum {
		return nil, fmt.Errorf("index checksum mismatch")
	}
	off := 0
	need := func(n int) bool { return off+n <= len(body) }
	if !need(4) {
		return nil, fmt.Errorf("index truncated")
	}
	count := int(binary.LittleEndian.Uint32(body[off:]))
	off += 4
	for i := 0; i < count; i++ {
		if !need(2) {
			return nil, fmt.Errorf("index truncated")
		}
		kl := int(binary.LittleEndian.Uint16(body[off:]))
		off += 2
		if !need(kl) {
			return nil, fmt.Errorf("index truncated")
		}
		key := string(body[off : off+kl])
		off += kl
		measurement, tags, err := model.ParseSeriesKey(key)
		if err != nil {
			return nil, fmt.Errorf("bad series key %q: %w", key, err)
		}
		ix.AddSeries(measurement, tags)
		if !need(4) {
			return nil, fmt.Errorf("index truncated")
		}
		bcount := int(binary.LittleEndian.Uint32(body[off:]))
		off += 4
		for j := 0; j < bcount; j++ {
			if !need(24) {
				return nil, fmt.Errorf("index truncated")
			}
			ref := BlockRef{
				Locator: int(int64(binary.LittleEndian.Uint64(body[off:]))),
				MinTs:   int64(binary.LittleEndian.Uint64(body[off+8:])),
				MaxTs:   int64(binary.LittleEndian.Uint64(body[off+16:])),
			}
			off += 24
			ix.SeriesBlocks[key] = append(ix.SeriesBlocks[key], ref)
		}
	}
	return ix, nil
}
