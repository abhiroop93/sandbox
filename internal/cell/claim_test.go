package cell

import "testing"

func TestClaimNameIsStablePerGeneration(t *testing.T) {
	id := "01234567-89ab-cdef-0123-456789abcdef"
	first := ClaimName(id, 1)
	if first != "sb-01234567-89ab-cdef-g1" {
		t.Fatalf("unexpected name %q", first)
	}
	if ClaimName(id, 1) != first {
		t.Fatal("same generation must always reuse its claim name")
	}
	if ClaimName(id, 2) == first {
		t.Fatal("new generation must have a new fenced claim name")
	}
}
