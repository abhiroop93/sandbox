package cell

import (
	"context"
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
)

func newLimitCell(max int) *Cell {
	scheme := runtime.NewScheme()
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		gvrTemplate: "SandboxTemplateList", gvrPool: "SandboxWarmPoolList"})
	return New(Config{Namespace: "sandboxes", RuntimeClass: "gvisor", CPU: "1", Memory: "2Gi", MaxTemplates: max}, dyn, nil)
}

func TestEnsureTemplateStopsAtCellImageLimit(t *testing.T) {
	c := newLimitCell(2)
	ctx := context.Background()
	for _, img := range []string{"a:1", "b:1"} {
		if _, err := c.EnsureTemplate(ctx, img, 1); err != nil {
			t.Fatalf("%s: %v", img, err)
		}
	}
	if _, err := c.EnsureTemplate(ctx, "a:1", 1); err != nil {
		t.Fatalf("an existing image must keep working at the limit: %v", err)
	}
	if _, err := c.EnsureTemplate(ctx, "c:1", 1); !errors.Is(err, ErrTemplateLimit) {
		t.Fatalf("a third image must hit the limit, got %v", err)
	}
}
