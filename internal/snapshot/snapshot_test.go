package snapshot

import (
	"strings"
	"testing"
)

func TestAutosavePrefixIsGenerationScopedAndOrdered(t *testing.T) {
	first := AutosavePrefix("id-1", 3, 2)
	second := AutosavePrefix("id-1", 3, 10)
	if !strings.HasPrefix(first, "ws/id-1/g3/auto/") {
		t.Fatalf("wrong generation scope: %s", first)
	}
	if first >= second {
		t.Fatalf("zero-padded revisions must sort lexically: %q >= %q", first, second)
	}
	if first == Prefix("id-1", 3) {
		t.Fatal("autosave must never target the explicit pause prefix")
	}
}
