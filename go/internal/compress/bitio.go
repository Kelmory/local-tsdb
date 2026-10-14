// Package compress implements the Gorilla-style columnar codecs:
// float64 XOR encoding and integer delta-of-delta with zigzag varints.
// All bit streams are MSB-first, byte-compatible with the Python reference.
package compress

import "fmt"

// ErrCorrupt signals malformed or truncated encoded data.
var ErrCorrupt = fmt.Errorf("corrupt compressed data")

// BitWriter accumulates bits MSB-first and flushes them into a byte buffer.
type BitWriter struct {
	buf   []byte
	acc   uint64 // pending bits, left-aligned
	nbits int    // number of pending bits (0..7 after each Write)
}

// Write appends the low nbits bits of value. nbits may be 0..64.
func (w *BitWriter) Write(value uint64, nbits int) {
	if nbits <= 0 {
		return
	}
	if nbits < 64 {
		value &= (uint64(1) << uint(nbits)) - 1
	}
	// Write in chunks of at most 32 bits so the accumulator stays bounded
	// (7 pending bits + 32 incoming <= 39 bits, far from overflow).
	for nbits > 0 {
		chunk := nbits
		if chunk > 32 {
			chunk = 32
		}
		shift := uint(nbits - chunk)
		top := value >> shift
		if chunk < 32 {
			top &= (uint64(1) << uint(chunk)) - 1
		}
		w.acc = w.acc<<uint(chunk) | top
		w.nbits += chunk
		for w.nbits >= 8 {
			w.nbits -= 8
			w.buf = append(w.buf, byte(w.acc>>uint(w.nbits)))
		}
		w.acc &= (uint64(1) << uint(w.nbits)) - 1
		nbits -= chunk
	}
}

// Flush pads the final partial byte with zero bits and returns all bytes.
func (w *BitWriter) Flush() []byte {
	if w.nbits > 0 {
		w.acc <<= uint(8 - w.nbits)
		w.buf = append(w.buf, byte(w.acc))
		w.acc = 0
		w.nbits = 0
	}
	return w.buf
}

// Len returns the number of bytes currently buffered (without padding).
func (w *BitWriter) Len() int { return len(w.buf) + (w.nbits+7)/8 }

// BitReader reads bits MSB-first out of a byte buffer.
type BitReader struct {
	data []byte
	pos  int // byte index
	bit  int // bit offset within current byte (0..7)
}

// Read reads nbits (0..64); returns ErrCorrupt when running out of data.
func (r *BitReader) Read(nbits int) (uint64, error) {
	if nbits < 0 || nbits > 64 {
		return 0, ErrCorrupt
	}
	var out uint64
	remaining := nbits
	for remaining > 0 {
		if r.pos >= len(r.data) {
			return 0, ErrCorrupt
		}
		avail := 8 - r.bit
		take := remaining
		if take > avail {
			take = avail
		}
		b := r.data[r.pos]
		bits := (b >> uint(8-r.bit-take)) & ((1 << uint(take)) - 1)
		out = out<<uint(take) | uint64(bits)
		r.bit += take
		remaining -= take
		if r.bit == 8 {
			r.bit = 0
			r.pos++
		}
	}
	return out, nil
}
