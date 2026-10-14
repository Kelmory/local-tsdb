// Package block implements the immutable columnar data block: the on-disk
// unit holding one series' samples over a contiguous run.
//
// Blob layout (little-endian, with a trailing CRC32):
//
//	u16  nfields
//	  repeat nfields (sorted by field name):
//	    u16 name_len ; utf-8 name ; u8 ftype
//	u32  point_count
//	i64  min_ts ; i64 max_ts
//	u32  ts_len ; ts_bytes            (int delta-of-delta)
//	  repeat nfields (same order):
//	    u32 col_len ; [null bitmap ; codec data]
//	u32  crc32 of all preceding bytes
//
// The null bitmap is ceil(point_count/8) bytes, LSB-first; bit i = 1 means
// row i has a value, 0 means NULL (the codec data holds a zero placeholder
// there to stay position-aligned).
package block

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"sort"

	"github.com/Kelmory/local-tsdb/internal/compress"
	"github.com/Kelmory/local-tsdb/internal/model"
)

// ErrFieldTypeConflict is returned when one field mixes value types.
var ErrFieldTypeConflict = fmt.Errorf("field type conflict")

// ErrCorrupt signals malformed or checksum-failing block bytes.
var ErrCorrupt = fmt.Errorf("corrupt data block")

// Row is one decoded sample; missing fields are nil.
type Row struct {
	Timestamp int64
	Fields    map[string]any
}

// Block is an in-memory columnar block for a single series.
type Block struct {
	timestamps []int64
	fieldOrder []string
	types      map[string]model.FieldType
	cols       map[string][]any
	nulls      map[string][]bool
}

// FromPoints builds a block from same-series points.
//
// Points are sorted by timestamp (stable), so min_ts/max_ts are the true
// time bounds. Duplicate timestamps merge into one row: later points win
// field-by-field and field sets are unioned (last-write-wins). A field
// absent on an individual point is stored as NULL. Conflicting value types
// for one field return ErrFieldTypeConflict.
func FromPoints(pts []model.Point) (*Block, error) {
	sorted := make([]model.Point, len(pts))
	copy(sorted, pts)
	stableSortByTs(sorted)

	// Merge duplicate timestamps: later points win field-by-field.
	merged := make([]model.Point, 0, len(sorted))
	for _, p := range sorted {
		if len(merged) > 0 && merged[len(merged)-1].Timestamp == p.Timestamp {
			prev := &merged[len(merged)-1]
			fields := make(map[string]any, len(prev.Fields)+len(p.Fields))
			for k, v := range prev.Fields {
				fields[k] = v
			}
			for k, v := range p.Fields {
				fields[k] = v
			}
			merged[len(merged)-1] = model.Point{
				Measurement: prev.Measurement, Tags: prev.Tags,
				Fields: fields, Timestamp: p.Timestamp,
			}
		} else {
			merged = append(merged, p)
		}
	}

	b := &Block{
		types: map[string]model.FieldType{},
		cols:  map[string][]any{},
		nulls: map[string][]bool{},
	}
	// Field set = sorted union.
	seen := map[string]bool{}
	for _, p := range merged {
		for name := range p.Fields {
			if !seen[name] {
				seen[name] = true
				b.fieldOrder = append(b.fieldOrder, name)
			}
		}
	}
	sortStrings(b.fieldOrder)

	// Infer a stable type per field from the first concrete value seen,
	// rejecting conflicting types on the way.
	for _, name := range b.fieldOrder {
		ftype := model.FieldType(0)
		for _, p := range merged {
			v, ok := p.Fields[name]
			if !ok || v == nil {
				continue
			}
			t, err := model.FieldTypeOf(v)
			if err != nil {
				return nil, err
			}
			if ftype == 0 {
				ftype = t
			} else if t != ftype {
				return nil, fmt.Errorf("%w for %q: cannot mix %s and %s values in one block",
					ErrFieldTypeConflict, name, ftype, t)
			}
		}
		if ftype == 0 {
			ftype = model.TypeStr
		}
		b.types[name] = ftype
		b.cols[name] = make([]any, 0, len(merged))
		b.nulls[name] = make([]bool, 0, len(merged))
	}

	for _, p := range merged {
		b.timestamps = append(b.timestamps, p.Timestamp)
		for _, name := range b.fieldOrder {
			v, ok := p.Fields[name]
			if !ok || v == nil {
				// Zero placeholder keeps the column codec position-aligned;
				// the presence bitmap marks the slot NULL.
				b.nulls[name] = append(b.nulls[name], true)
				v = zeroOf(b.types[name])
			} else {
				b.nulls[name] = append(b.nulls[name], false)
				v = normalize(v, b.types[name])
			}
			b.cols[name] = append(b.cols[name], v)
		}
	}
	return b, nil
}

func zeroOf(ft model.FieldType) any {
	switch ft {
	case model.TypeInt:
		return int64(0)
	case model.TypeFloat:
		return float64(0)
	case model.TypeBool:
		return false
	default:
		return ""
	}
}

func normalize(v any, ft model.FieldType) any {
	switch ft {
	case model.TypeInt:
		switch x := v.(type) {
		case int:
			return int64(x)
		case int64:
			return x
		case uint64:
			return int64(x)
		}
	case model.TypeFloat:
		switch x := v.(type) {
		case float64:
			return x
		case float32:
			return float64(x)
		}
	}
	return v
}

func stableSortByTs(pts []model.Point) {
	sort.SliceStable(pts, func(i, j int) bool { return pts[i].Timestamp < pts[j].Timestamp })
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// FieldNames returns the field names in stable (sorted) order.
func (b *Block) FieldNames() []string { return append([]string(nil), b.fieldOrder...) }

// FieldType returns the stored type tag of a field.
func (b *Block) FieldType(name string) model.FieldType { return b.types[name] }

// Len returns the number of rows.
func (b *Block) Len() int { return len(b.timestamps) }

// MinTs returns the smallest timestamp; (0, false) when empty.
func (b *Block) MinTs() (int64, bool) {
	if len(b.timestamps) == 0 {
		return 0, false
	}
	return b.timestamps[0], true
}

// MaxTs returns the largest timestamp; (0, false) when empty.
func (b *Block) MaxTs() (int64, bool) {
	if len(b.timestamps) == 0 {
		return 0, false
	}
	return b.timestamps[len(b.timestamps)-1], true
}

// Timestamps exposes the shared timestamp column.
func (b *Block) Timestamps() []int64 { return b.timestamps }

// IsNull reports whether row i's field is NULL.
func (b *Block) IsNull(name string, i int) bool { return b.nulls[name][i] }

// Value returns row i's raw field value (zero placeholder when NULL).
func (b *Block) Value(name string, i int) any { return b.cols[name][i] }

// Rows yields (timestamp, fields) in time order; absent fields are nil.
func (b *Block) Rows() []Row {
	out := make([]Row, len(b.timestamps))
	for i, ts := range b.timestamps {
		fields := make(map[string]any, len(b.fieldOrder))
		for _, name := range b.fieldOrder {
			if b.nulls[name][i] {
				fields[name] = nil
			} else {
				fields[name] = b.cols[name][i]
			}
		}
		out[i] = Row{Timestamp: ts, Fields: fields}
	}
	return out
}

func packNulls(present []bool) []byte {
	out := make([]byte, (len(present)+7)/8)
	for i, ok := range present {
		if ok {
			out[i>>3] |= 1 << uint(i&7)
		}
	}
	return out
}

// ToBytes serializes the block (with trailing CRC32).
func (b *Block) ToBytes() []byte {
	out := make([]byte, 0, 64)
	out = binary.LittleEndian.AppendUint16(out, uint16(len(b.fieldOrder)))
	for _, name := range b.fieldOrder {
		out = binary.LittleEndian.AppendUint16(out, uint16(len(name)))
		out = append(out, name...)
		out = append(out, byte(b.types[name]))
	}
	count := len(b.timestamps)
	out = binary.LittleEndian.AppendUint32(out, uint32(count))
	minTs, maxTs := int64(0), int64(0)
	if count > 0 {
		minTs, maxTs = b.timestamps[0], b.timestamps[count-1]
	}
	out = binary.LittleEndian.AppendUint64(out, uint64(minTs))
	out = binary.LittleEndian.AppendUint64(out, uint64(maxTs))

	tsBytes := compress.EncodeIntsDod(b.timestamps)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(tsBytes)))
	out = append(out, tsBytes...)

	for _, name := range b.fieldOrder {
		// Pack presence (bit=1 ⇒ value present); nulls[] is inverted.
		present := make([]bool, len(b.nulls[name]))
		for i, isNull := range b.nulls[name] {
			present[i] = !isNull
		}
		body := packNulls(present)
		body = append(body, encodeColumn(b.types[name], b.cols[name])...)
		out = binary.LittleEndian.AppendUint32(out, uint32(len(body)))
		out = append(out, body...)
	}
	return binary.LittleEndian.AppendUint32(out, crc32.ChecksumIEEE(out))
}

func encodeColumn(ft model.FieldType, values []any) []byte {
	switch ft {
	case model.TypeFloat:
		f := make([]float64, len(values))
		for i, v := range values {
			f[i] = v.(float64)
		}
		return compress.EncodeFloats(f)
	case model.TypeInt:
		n := make([]int64, len(values))
		for i, v := range values {
			n[i] = v.(int64)
		}
		return compress.EncodeIntsDod(n)
	}
	// bool / str -> raw.
	out := make([]byte, 0, len(values)*8)
	for _, v := range values {
		if ft == model.TypeBool {
			if v.(bool) {
				out = append(out, 1)
			} else {
				out = append(out, 0)
			}
		} else {
			out = binary.LittleEndian.AppendUint32(out, uint32(len(v.(string))))
			out = append(out, v.(string)...)
		}
	}
	return out
}

// FromBytes deserializes a block and verifies its trailing CRC32.
func FromBytes(data []byte) (*Block, error) {
	if len(data) < 4 {
		return nil, ErrCorrupt
	}
	body, sum := data[:len(data)-4], binary.LittleEndian.Uint32(data[len(data)-4:])
	if crc32.ChecksumIEEE(body) != sum {
		return nil, fmt.Errorf("%w: checksum mismatch", ErrCorrupt)
	}
	off := 0
	need := func(n int) error {
		if off+n > len(body) {
			return fmt.Errorf("%w: truncated at offset %d", ErrCorrupt, off)
		}
		return nil
	}

	if err := need(2); err != nil {
		return nil, err
	}
	nfields := int(binary.LittleEndian.Uint16(body[off:]))
	off += 2
	names := make([]string, 0, nfields)
	types := map[string]model.FieldType{}
	for i := 0; i < nfields; i++ {
		name, next, err := model.UnpackName(body, off)
		if err != nil {
			return nil, ErrCorrupt
		}
		off = next
		if err := need(1); err != nil {
			return nil, err
		}
		ft := model.FieldType(body[off])
		off++
		if ft < model.TypeInt || ft > model.TypeStr {
			return nil, fmt.Errorf("%w: bad field type %d", ErrCorrupt, ft)
		}
		names = append(names, name)
		types[name] = ft
	}
	if err := need(4 + 16); err != nil {
		return nil, err
	}
	count := int(binary.LittleEndian.Uint32(body[off:]))
	off += 4
	minTs := int64(binary.LittleEndian.Uint64(body[off:]))
	off += 8
	maxTs := int64(binary.LittleEndian.Uint64(body[off:]))
	off += 8
	_ = minTs
	_ = maxTs

	if err := need(4); err != nil {
		return nil, err
	}
	tsLen := int(binary.LittleEndian.Uint32(body[off:]))
	off += 4
	if tsLen < 0 || off+tsLen > len(body) {
		return nil, ErrCorrupt
	}
	tsBytes := body[off : off+tsLen]
	off += tsLen
	var timestamps []int64
	if count > 0 {
		decoded, err := compress.DecodeIntsDod(tsBytes)
		if err != nil {
			return nil, err
		}
		if len(decoded) < count {
			return nil, fmt.Errorf("%w: timestamp count mismatch", ErrCorrupt)
		}
		timestamps = decoded[:count]
	}

	cols := map[string][]any{}
	nulls := map[string][]bool{}
	for _, name := range names {
		if err := need(4); err != nil {
			return nil, err
		}
		colLen := int(binary.LittleEndian.Uint32(body[off:]))
		off += 4
		if colLen < 0 || off+colLen > len(body) {
			return nil, ErrCorrupt
		}
		chunk := body[off : off+colLen]
		off += colLen
		nbitmap := (count + 7) / 8
		if len(chunk) < nbitmap {
			return nil, ErrCorrupt
		}
		bitmap, colData := chunk[:nbitmap], chunk[nbitmap:]
		col, err := decodeColumn(types[name], colData, count)
		if err != nil {
			return nil, err
		}
		cols[name] = col
		nulls[name] = make([]bool, count)
		for i := 0; i < count; i++ {
			nulls[name][i] = bitmap[i>>3]>>(uint(i)&7)&1 == 0
		}
	}

	b := &Block{
		timestamps: timestamps,
		fieldOrder: names,
		types:      types,
		cols:       cols,
		nulls:      nulls,
	}
	return b, nil
}

func decodeColumn(ft model.FieldType, data []byte, count int) ([]any, error) {
	switch ft {
	case model.TypeFloat:
		f, err := compress.DecodeFloats(data, count)
		if err != nil {
			return nil, err
		}
		out := make([]any, count)
		for i, v := range f {
			out[i] = v
		}
		return out, nil
	case model.TypeInt:
		n, err := compress.DecodeIntsDod(data)
		if err != nil {
			return nil, err
		}
		if len(n) < count {
			return nil, ErrCorrupt
		}
		out := make([]any, count)
		for i := 0; i < count; i++ {
			out[i] = n[i]
		}
		return out, nil
	}
	out := make([]any, 0, count)
	off := 0
	for i := 0; i < count; i++ {
		if ft == model.TypeBool {
			if off >= len(data) {
				return nil, ErrCorrupt
			}
			out = append(out, data[off] != 0)
			off++
		} else {
			if off+4 > len(data) {
				return nil, ErrCorrupt
			}
			ln := int(binary.LittleEndian.Uint32(data[off:]))
			off += 4
			if ln < 0 || off+ln > len(data) {
				return nil, ErrCorrupt
			}
			out = append(out, string(data[off:off+ln]))
			off += ln
		}
	}
	return out, nil
}
