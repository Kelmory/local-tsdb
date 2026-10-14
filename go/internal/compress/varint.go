package compress

// Zigzag maps signed ints to non-negative ints preserving small-magnitude
// order: (n << 1) ^ (n >> 63).
func Zigzag(n int64) uint64 {
	return uint64(n)<<1 ^ uint64(n>>63)
}

// Unzigzag is the inverse of Zigzag.
func Unzigzag(z uint64) int64 {
	return int64(z>>1) ^ -int64(z&1)
}

// PutVarint appends the unsigned LEB128 encoding of n to buf.
func PutVarint(buf []byte, n uint64) []byte {
	for {
		b := byte(n & 0x7F)
		n >>= 7
		if n != 0 {
			buf = append(buf, b|0x80)
		} else {
			return append(buf, b)
		}
	}
}

// Varint returns the unsigned LEB128 encoding of n.
func Varint(n uint64) []byte {
	return PutVarint(make([]byte, 0, 10), n)
}

// ReadVarint decodes an unsigned LEB128 varint at buf[pos]; returns the value
// and the next position. Guards against runaway shifts on corrupt data.
func ReadVarint(buf []byte, pos int) (uint64, int, error) {
	var result uint64
	shift := uint(0)
	for {
		if pos >= len(buf) {
			return 0, pos, ErrCorrupt
		}
		b := buf[pos]
		pos++
		if shift >= 64 {
			return 0, pos, ErrCorrupt
		}
		result |= uint64(b&0x7F) << shift
		if b&0x80 == 0 {
			return result, pos, nil
		}
		shift += 7
	}
}
