package lifecycle

import (
	"testing"
	"time"

	"github.com/abhiroop93/sandbox/internal/store"
)

func TestOperationPayloadRoundTrip(t *testing.T) {
	want := operationPayload{Hydrate: true, Emergency: true, Event: "paused", TargetPrefix: "ws/id/g2/"}
	job := &store.Job{Payload: encodePayload(want)}
	if got := decodePayload(job); got != want {
		t.Fatalf("payload changed: got %+v want %+v", got, want)
	}
	if got := decodePayload(&store.Job{}); got != (operationPayload{}) {
		t.Fatalf("missing payload must decode safely: %+v", got)
	}
}

func TestNextAutosaveUsesPersistableUTCDeadline(t *testing.T) {
	m := &Manager{AutosaveInterval: time.Minute}
	var patch store.Patch
	m.setNextAutosave(&patch)
	if patch.AutosaveAt == nil || *patch.AutosaveAt == nil {
		t.Fatal("enabled autosave must schedule a deadline")
	}
	when := **patch.AutosaveAt
	if when.Location() != time.UTC || time.Until(when) < 50*time.Second || time.Until(when) > 70*time.Second {
		t.Fatalf("unexpected deadline %s", when)
	}
	m.AutosaveInterval = 0
	patch = store.Patch{}
	m.setNextAutosave(&patch)
	if patch.AutosaveAt != nil {
		t.Fatal("disabled autosave must not schedule work")
	}
}
