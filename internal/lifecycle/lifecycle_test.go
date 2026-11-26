package lifecycle

import (
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/abhiroop93/sandbox/internal/cell"
	"github.com/abhiroop93/sandbox/internal/store"
)

func np(defaultAction string, rules ...[3]any) *NetworkPolicy {
	p := &NetworkPolicy{DefaultAction: defaultAction}
	for _, r := range rules {
		rule := struct {
			Action string `json:"action"`
			Target string `json:"target,omitempty"`
			Ports  []int  `json:"ports,omitempty"`
		}{Action: r[0].(string), Target: r[1].(string)}
		if r[2] != nil {
			rule.Ports = r[2].([]int)
		}
		p.Egress = append(p.Egress, rule)
	}
	return p
}

func TestValidateNetwork(t *testing.T) {
	cases := []struct {
		name string
		p    *NetworkPolicy
		ok   bool
	}{
		{"nil policy = template default", nil, true},
		{"deny all", np("deny"), true},
		{"allow all", np("allow"), true},
		{"allow cidr", np("deny", [3]any{"allow", "203.0.113.0/24", []int{443}}), true},
		{"allow single ip", np("deny", [3]any{"allow", "198.51.100.7", nil}), true},
		{"fqdn needs cilium", np("deny", [3]any{"allow", "pypi.org", nil}), false},
		{"wildcard fqdn", np("deny", [3]any{"allow", "*.github.com", nil}), false},
		{"explicit deny rule unsupported", np("allow", [3]any{"deny", "10.0.0.0/8", nil}), false},
		{"bad default action", np("block"), false},
		{"empty target", np("deny", [3]any{"allow", "", nil}), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateNetwork(c.p)
			if c.ok && err != nil {
				t.Fatalf("expected ok, got %v", err)
			}
			if !c.ok && (err == nil || !errors.Is(err, ErrInvalid)) {
				t.Fatalf("expected ErrInvalid, got %v", err)
			}
		})
	}
}

func TestEncodeURLsRoundTrip(t *testing.T) {
	if got := encodeURLs([]string{"http://a/1", "http://b/2"}); got == "" {
		t.Fatal("empty encoding")
	}
	if lastLine("x\ny\n{\"chunks\":2}\n") != `{"chunks":2}` {
		t.Fatal("lastLine should return the final JSON line")
	}
}

func TestPodVerdict(t *testing.T) {
	ip := "10.0.0.5"
	sb := &store.Sandbox{ID: "sb-1", PodIP: &ip}
	ok := cell.PodInfo{Name: "p", IP: ip, Phase: corev1.PodRunning, Restarts: 1, BoundRestarts: 1}
	with := func(f func(*cell.PodInfo)) cell.PodInfo { p := ok; f(&p); return p }
	cases := []struct {
		name  string
		p     cell.PodInfo
		found bool
		want  verdict
	}{
		{"healthy", ok, true, podOK},
		{"pod gone (node crash, force delete)", ok, false, podLost},
		{"graceful shutdown (drain/eviction)", with(func(p *cell.PodInfo) { p.Terminating = true }), true, podTerminating},
		{"pod failed", with(func(p *cell.PodInfo) { p.Phase = corev1.PodFailed }), true, podLost},
		{"replaced under the same name", with(func(p *cell.PodInfo) { p.IP = "10.0.0.9" }), true, podLost},
		{"container restarted (OOM)", with(func(p *cell.PodInfo) { p.Restarts = 2 }), true, podRestarted},
		{"restart count not recorded yet (just started)", with(func(p *cell.PodInfo) { p.Restarts = 3; p.BoundRestarts = -1 }), true, podOK},
		{"terminating wins over restart", with(func(p *cell.PodInfo) { p.Terminating = true; p.Restarts = 5 }), true, podTerminating},
	}
	for _, c := range cases {
		if got, _ := podVerdict(sb, c.p, c.found); got != c.want {
			t.Errorf("%s: got %d want %d", c.name, got, c.want)
		}
	}
}
