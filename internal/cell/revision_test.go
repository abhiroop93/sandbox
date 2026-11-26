package cell

import "testing"

func TestTemplateRevisionIncludesEffectivePodConfiguration(t *testing.T) {
	base := New(Config{RuntimeClass: "gvisor", ExecdImage: "execd:v1", HelperImage: "helper:v1",
		CPU: "1", Memory: "2Gi", DiskRequest: "512Mi", DiskLimit: "6Gi"}, nil, nil)
	if got := base.templateRevision(); got != base.templateRevision() {
		t.Fatal("unchanged configuration must have a stable revision")
	}
	changed := *base
	changed.cfg.Memory = "1Gi"
	if changed.templateRevision() == base.templateRevision() {
		t.Fatal("memory change must invalidate the old template")
	}
	changed = *base
	changed.cfg.DiskLimit = "8Gi"
	if changed.templateRevision() == base.templateRevision() {
		t.Fatal("disk change must invalidate the old template")
	}
}
