package abi

import (
	"math"
	"math/rand"
	"testing"
)

// TestPackResultBitLayout asserts the concrete bit layout of PackResult's
// return value: the high 32 bits must be the pointer and the low 32 bits
// must be the length. A test that only checks UnpackResult(PackResult(p, l))
// == (p, l) would still pass if the halves were swapped inside both
// functions consistently, so this test inspects the packed uint64 directly.
func TestPackResultBitLayout(t *testing.T) {
	tests := []struct {
		name   string
		ptr    uint32
		length uint32
	}{
		{"zero", 0, 0},
		{"one", 1, 1},
		{"max", math.MaxUint32, math.MaxUint32},
		{"distinct halves", 0xDEADBEEF, 0x12345678},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			packed := PackResult(tt.ptr, tt.length)

			if got := uint32(packed >> 32); got != tt.ptr {
				t.Errorf("PackResult(%d, %d) high 32 bits = %d, want ptr %d", tt.ptr, tt.length, got, tt.ptr)
			}
			if got := uint32(packed); got != tt.length {
				t.Errorf("PackResult(%d, %d) low 32 bits = %d, want length %d", tt.ptr, tt.length, got, tt.length)
			}
		})
	}
}

// TestPackResultZero asserts that PackResult(0, 0) is the zero value. The
// host relies on outLen == 0 (equivalently packed == 0 when ptr is also 0)
// to mean "no return body", not an error.
func TestPackResultZero(t *testing.T) {
	if got := PackResult(0, 0); got != 0 {
		t.Errorf("PackResult(0, 0) = %d, want 0", got)
	}
}

// TestUnpackResultRoundTrip asserts UnpackResult(PackResult(p, l)) == (p, l)
// for fixed edge cases and randomly generated pairs.
func TestUnpackResultRoundTrip(t *testing.T) {
	cases := [][2]uint32{
		{0, 0},
		{1, 1},
		{math.MaxUint32, math.MaxUint32},
		{0xDEADBEEF, 0x12345678},
	}

	rng := rand.New(rand.NewSource(42))
	for i := 0; i < 20; i++ {
		cases = append(cases, [2]uint32{rng.Uint32(), rng.Uint32()})
	}

	for _, c := range cases {
		ptr, length := c[0], c[1]
		packed := PackResult(ptr, length)
		gotPtr, gotLength := UnpackResult(packed)
		if gotPtr != ptr || gotLength != length {
			t.Errorf("UnpackResult(PackResult(%d, %d)) = (%d, %d), want (%d, %d)", ptr, length, gotPtr, gotLength, ptr, length)
		}
	}
}
