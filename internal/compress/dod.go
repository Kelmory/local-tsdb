package compress

// EncodeIntsDod encodes a counter column with delta-of-delta + zigzag varints.
//
// Layout: a plain (non-zigzag) varint count header, the first value as a
// zigzag varint, then one zigzag varint per subsequent sample holding the
// second difference (the change in the consecutive first differences).
func EncodeIntsDod(values []int64) []byte {
	if len(values) == 0 {
		return nil
	}
	out := Varint(uint64(len(values)))
	push := func(n int64) { out = PutVarint(out, Zigzag(n)) }
	prevVal := int64(0)
	prevDelta := int64(0)
	for idx, v := range values {
		if idx == 0 {
			push(v) // raw first value
		} else {
			delta := v - prevVal
			push(delta - prevDelta) // second difference
			prevDelta = delta
		}
		prevVal = v
	}
	return out
}

// DecodeIntsDod is the inverse of EncodeIntsDod.
func DecodeIntsDod(data []byte) ([]int64, error) {
	if len(data) == 0 {
		return nil, nil
	}
	count, pos, err := ReadVarint(data, 0)
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, nil
	}
	out := make([]int64, count)
	take := func() (int64, error) {
		z, next, err := ReadVarint(data, pos)
		if err != nil {
			return 0, err
		}
		pos = next
		return Unzigzag(z), nil
	}
	prevVal, err := take() // raw first value
	if err != nil {
		return nil, err
	}
	out[0] = prevVal
	prevDelta := int64(0)
	for i := 1; i < int(count); i++ {
		dd, err := take()
		if err != nil {
			return nil, err
		}
		delta := dd + prevDelta
		prevDelta = delta
		prevVal += delta
		out[i] = prevVal
	}
	return out, nil
}
