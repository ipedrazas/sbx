package cache

import (
	"encoding/binary"
	"math/bits"
)

// rustHasher reproduces Rust's std DefaultHasher (SipHash-1-3, zero keys) so
// auto-generated cache ids match the Rust implementation.
type rustHasher struct {
	buf []byte
}

// writeStr mirrors `impl Hash for str`: the bytes followed by a 0xff terminator.
func (h *rustHasher) writeStr(s string) {
	h.buf = append(h.buf, s...)
	h.buf = append(h.buf, 0xff)
}

func (h *rustHasher) finish() uint64 { return sip13(h.buf) }

func sip13(msg []byte) uint64 {
	v0 := uint64(0x736f6d6570736575)
	v1 := uint64(0x646f72616e646f6d)
	v2 := uint64(0x6c7967656e657261)
	v3 := uint64(0x7465646279746573)
	round := func() {
		v0 += v1
		v1 = bits.RotateLeft64(v1, 13)
		v1 ^= v0
		v0 = bits.RotateLeft64(v0, 32)
		v2 += v3
		v3 = bits.RotateLeft64(v3, 16)
		v3 ^= v2
		v0 += v3
		v3 = bits.RotateLeft64(v3, 21)
		v3 ^= v0
		v2 += v1
		v1 = bits.RotateLeft64(v1, 17)
		v1 ^= v2
		v2 = bits.RotateLeft64(v2, 32)
	}
	n := len(msg)
	i := 0
	for ; i+8 <= n; i += 8 {
		m := binary.LittleEndian.Uint64(msg[i:])
		v3 ^= m
		round()
		v0 ^= m
	}
	b := uint64(n) << 56
	for j := 0; i+j < n; j++ {
		b |= uint64(msg[i+j]) << (8 * j)
	}
	v3 ^= b
	round()
	v0 ^= b
	v2 ^= 0xff
	round()
	round()
	round()
	return v0 ^ v1 ^ v2 ^ v3
}

func (h *rustHasher) writeUsize(v int) {
	h.buf = binary.LittleEndian.AppendUint64(h.buf, uint64(v))
}
