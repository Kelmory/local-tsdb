package compress

import (
	"math"
	"math/bits"
)

// EncodeFloats encodes a column of doubles with the Gorilla XOR scheme.
//
// Layout: the first value is stored raw (64 bits); each later value stores
// one flag bit and, when it changed, an 8-bit leading-zero count ("skip")
// plus the significant trailing bits of the XOR.
func EncodeFloats(values []float64) []byte {
	if len(values) == 0 {
		return nil
	}
	w := &BitWriter{}
	prevBits := math.Float64bits(values[0])
	w.Write(prevBits, 64)
	for _, v := range values[1:] {
		curBits := math.Float64bits(v)
		xor := prevBits ^ curBits
		if xor == 0 {
			w.Write(0, 1) // unchanged -> single zero bit
		} else {
			w.Write(1, 1) // changed flag
			skip := leadingZeros64(xor)
			w.Write(uint64(skip), 8)
			w.Write(xor, 64-skip) // top bits are zero by construction
		}
		prevBits = curBits
	}
	return w.Flush()
}

// DecodeFloats is the inverse of EncodeFloats.
func DecodeFloats(data []byte, count int) ([]float64, error) {
	if len(data) == 0 || count <= 0 {
		return make([]float64, maxInt(count, 0)), nil
	}
	r := &BitReader{data: data}
	out := make([]float64, count)
	prevBits, err := r.Read(64)
	if err != nil {
		return nil, err
	}
	out[0] = math.Float64frombits(prevBits)
	for i := 1; i < count; i++ {
		flag, err := r.Read(1)
		if err != nil {
			return nil, err
		}
		var curBits uint64
		if flag == 0 {
			curBits = prevBits
		} else {
			skip, err := r.Read(8)
			if err != nil {
				return nil, err
			}
			width := 64 - int(skip)
			low, err := r.Read(width)
			if err != nil {
				return nil, err
			}
			// The top `skip` bits of the xor are zero by construction, so
			// the stored trailing bits *are* the full xor value.
			curBits = prevBits ^ low
		}
		out[i] = math.Float64frombits(curBits)
		prevBits = curBits
	}
	return out, nil
}

func leadingZeros64(x uint64) int { return bits.LeadingZeros64(x) }

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
