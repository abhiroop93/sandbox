package lifecycle

import (
	"errors"
	"testing"
)

func TestValidateResourceMap(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values map[string]string
		ok     bool
	}{
		{"SDK defaults", map[string]string{"cpu": "1", "memory": "2Gi"}, true},
		{"equivalent CPU quantity", map[string]string{"cpu": "1000m"}, true},
		{"missing values use fixed class", nil, true},
		{"custom CPU rejected", map[string]string{"cpu": "4"}, false},
		{"custom memory rejected", map[string]string{"memory": "1Gi"}, false},
		{"unsupported dimension", map[string]string{"nvidia.com/gpu": "1"}, false},
		{"invalid quantity", map[string]string{"memory": "lots"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateResourceMap("resourceLimits", tc.values, "1", "2Gi")
			if tc.ok && err != nil {
				t.Fatal(err)
			}
			if !tc.ok && !errors.Is(err, ErrInvalid) {
				t.Fatalf("expected ErrInvalid, got %v", err)
			}
		})
	}
}
