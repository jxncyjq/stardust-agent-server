package gateway

import "testing"

func TestHashIDStableAndAnonymized(t *testing.T) {
	a := HashID("123456789")
	b := HashID("123456789")
	if a != b {
		t.Fatalf("HashID not stable: %q vs %q", a, b)
	}
	if a == "123456789" || a == "" {
		t.Fatalf("HashID did not anonymize: %q", a)
	}
	if HashID("123456789") == HashID("987654321") {
		t.Fatalf("HashID collided on distinct inputs")
	}
	if len(a) != 16 {
		t.Fatalf("HashID len = %d, want 16", len(a))
	}
}
