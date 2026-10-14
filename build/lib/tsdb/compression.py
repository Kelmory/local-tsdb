"""Columnar compression codecs.

Implements the two core encodings popularised by Facebook's *Gorilla* paper
and used by InfluxDB's TSM engine:

* **Float64 XOR encoding** -- each value is XORed with the previous one; a run
  of unchanged values collapses to a single bit, and changed values store only
  their significant trailing bits after a "skip" prefix. Ideal for slowly
  drifting gauges (temperatures, voltages).
* **Integer delta-of-delta** -- counters are stored as second differences with
  zig-zag + varint packing, so small increments occupy very few bytes.

Both codecs are pure functions over Python sequences and round-trip exactly,
which makes them trivially unit-testable and safe to persist on disk.
"""
from __future__ import annotations

import struct
from typing import List


# --------------------------------------------------------------------------- #
# Bit-level I/O helpers (MSB-first)
# --------------------------------------------------------------------------- #
class _BitWriter:
    """Accumulates bits MSB-first and flushes them into a ``bytes`` buffer."""

    def __init__(self) -> None:
        self._buf = bytearray()
        self._bits = 0
        self._acc = 0

    def write(self, value: int, nbits: int) -> None:
        """Append the low ``nbits`` bits of ``value``."""
        mask = (1 << nbits) - 1
        for i in range(nbits - 1, -1, -1):
            bit = (value >> i) & 1
            self._acc = (self._acc << 1) | bit
            self._bits += 1
            if self._bits == 8:
                self._buf.append(self._acc)
                self._acc = 0
                self._bits = 0

    def flush(self) -> bytes:
        """Pad the final partial byte with zero bits and return all bytes."""
        if self._bits:
            self._acc <<= (8 - self._bits)
            self._buf.append(self._acc & 0xFF)
            self._acc = 0
            self._bits = 0
        return bytes(self._buf)


class _BitReader:
    """Reads bits MSB-first out of a ``bytes`` buffer."""

    def __init__(self, data: bytes) -> None:
        self._data = data
        self._pos = 0          # byte index
        self._bit = 0          # bit offset within current byte (0..7)

    def read(self, nbits: int) -> int:
        value = 0
        for _ in range(nbits):
            if self._bit == 8:
                self._bit = 0
                self._pos += 1
            bit = (self._data[self._pos] >> (7 - self._bit)) & 1
            self._bit += 1
            value = (value << 1) | bit
        return value


def _float_to_bits(x: float) -> int:
    """Interpret a double as its raw 64-bit pattern."""
    return struct.unpack(">Q", struct.pack(">d", x))[0]


def _bits_to_float(b: int) -> float:
    return struct.unpack(">d", struct.pack(">Q", b))[0]


# --------------------------------------------------------------------------- #
# Float64 XOR (Gorilla "gauges") codec
# --------------------------------------------------------------------------- #
def encode_floats(values: List[float]) -> bytes:
    """Encode a column of doubles with the Gorilla XOR scheme.

    Layout: ``count`` (varint) then per-value bit stream. The first value is
    stored raw; each later value stores one flag bit and, when it changed, a
    skip prefix plus the significant trailing bits.
    """
    if not values:
        return b""
    w = _BitWriter()
    # First value raw (64 bits).
    prev_bits = _float_to_bits(values[0])
    w.write(prev_bits, 64)
    for v in values[1:]:
        cur_bits = _float_to_bits(v)
        xor = prev_bits ^ cur_bits
        if xor == 0:
            w.write(0, 1)          # unchanged -> single zero bit
        else:
            w.write(1, 1)          # changed flag
            skip = _leading_zeros(xor, 64)
            w.write(skip, 8)       # leading-zero count (0..63 fits a byte)
            width = 64 - skip
            w.write(xor & ((1 << width) - 1), width)
        prev_bits = cur_bits
    return w.flush()


def decode_floats(data: bytes, count: int) -> List[float]:
    """Inverse of :func:`encode_floats`."""
    if not data or count <= 0:
        return []
    r = _BitReader(data)
    out: List[float] = [None] * count  # type: ignore[list-item]
    prev_bits = r.read(64)
    out[0] = _bits_to_float(prev_bits)
    for i in range(1, count):
        flag = r.read(1)
        if flag == 0:
            cur_bits = prev_bits
        else:
            skip = r.read(8)
            width = 64 - skip
            low = r.read(width)
            # The top ``skip`` bits of the xor are zero by construction, so the
            # stored trailing bits *are* the full xor value; recover via XOR.
            cur_bits = prev_bits ^ low
        out[i] = _bits_to_float(cur_bits)
        prev_bits = cur_bits
    return out


def _leading_zeros(x: int, total: int) -> int:
    if x == 0:
        return total
    n = 0
    for i in range(total - 1, -1, -1):
        if (x >> i) & 1:
            break
        n += 1
    return n


# --------------------------------------------------------------------------- #
# Integer delta-of-delta codec (Gorilla "counters")
# --------------------------------------------------------------------------- #
def _zigzag(n: int) -> int:
    """Map signed ints to non-negative ints preserving small-magnitude order."""
    return (n << 1) ^ (n >> 63)


def _unzigzag(z: int) -> int:
    return (z >> 1) ^ -(z & 1)


def _varint(n: int) -> bytes:
    """Unsigned LEB128 varint."""
    out = bytearray()
    while True:
        b = n & 0x7F
        n >>= 7
        if n:
            out.append(b | 0x80)
        else:
            out.append(b)
            return bytes(out)


def _read_varint(buf: bytes, pos: int):
    result = 0
    shift = 0
    while True:
        b = buf[pos]
        pos += 1
        result |= (b & 0x7F) << shift
        if not (b & 0x80):
            break
        shift += 7
    return result, pos


def encode_ints_dod(values: List[int]) -> bytes:
    """Encode a counter column with delta-of-delta + zig-zag varints.

    Layout: ``count`` (varint), the first value raw, then one zig-zag varint
    per subsequent sample holding the *second* difference (the change in the
    consecutive first differences). Small steady increments therefore pack to
    a handful of bytes each.
    """
    if not values:
        return b""
    out = bytearray()
    out += _varint(len(values))

    def push(n: int) -> None:
        out.extend(_varint(_zigzag(n)))

    prev_val = 0
    prev_delta = 0
    for idx, v in enumerate(values):
        if idx == 0:
            push(v)                 # raw first value
        else:
            delta = v - prev_val
            push(delta - prev_delta)  # second difference (first diff when idx==1)
            prev_delta = delta
        prev_val = v
    return bytes(out)


def decode_ints_dod(data: bytes) -> List[int]:
    """Inverse of :func:`encode_ints_dod`."""
    if not data:
        return []
    pos = 0

    def take() -> int:
        nonlocal pos
        z, pos = _read_varint(data, pos)
        return _unzigzag(z)

    # ``count`` is a plain (non zig-zag) varint header.
    count, pos = _read_varint(data, 0)
    if count == 0:
        return []
    out: List[int] = [0] * count
    prev_val = take()               # raw first value
    out[0] = prev_val
    prev_delta = 0
    for i in range(1, count):
        dd = take()
        delta = dd + prev_delta
        prev_delta = delta
        val = prev_val + delta
        prev_val = val
        out[i] = val
    return out
