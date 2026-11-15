package cell

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

var dns1123 = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func TestTemplateNameIsStableAndValid(t *testing.T) {
	for _, img := range []string{"python:3.12-slim", "docker.io/library/python:3.12", "ghcr.io/Org/Some_Image:v1.2.3@sha256:abc",
		strings.Repeat("x", 300)} {
		n := TemplateName(img)
		if n != TemplateName(img) {
			t.Fatalf("not deterministic for %q", img)
		}
		if len(n) > 63 || !dns1123.MatchString(n) {
			t.Fatalf("invalid k8s name %q for %q", n, img)
		}
	}
	if TemplateName("python:3.12") == TemplateName("python:3.13") {
		t.Fatal("different images must map to different templates")
	}
}

func testCell() (*Cell, *fake.Clientset) {
	k := fake.NewSimpleClientset()
	return New(Config{Namespace: "sb", SystemNamespace: "sys", RuntimeClass: "gvisor", ExecdImage: "execd:1", HelperImage: "srvm:1",
		CPU: "1", Memory: "1Gi", DiskRequest: "512Mi", DiskLimit: "6Gi", ObjectStorePort: 8080, ObjectStoreLabel: "s3"}, nil, k), k
}

// The pod template is the security boundary for untrusted code: lock its invariants.
func TestPodTemplateHardening(t *testing.T) {
	c, _ := testCell()
	pt := c.podTemplate("img-x", "python:3.12")
	spec := pt["spec"].(map[string]any)
	if spec["runtimeClassName"] != "gvisor" {
		t.Fatal("sandbox pods must use the gVisor RuntimeClass")
	}
	if spec["automountServiceAccountToken"] != false {
		t.Fatal("service account token must not be mounted")
	}
	sc := spec["securityContext"].(map[string]any)
	if sc["runAsNonRoot"] != true || sc["runAsUser"] != int64(1000) {
		t.Fatal("sandbox must run as non-root uid 1000")
	}
	ctr := spec["containers"].([]any)[0].(map[string]any)
	csc := ctr["securityContext"].(map[string]any)
	if csc["allowPrivilegeEscalation"] != false {
		t.Fatal("privilege escalation must be disabled")
	}
	envs := map[string]string{}
	for _, e := range ctr["env"].([]any) {
		m := e.(map[string]any)
		envs[m["name"].(string)] = m["value"].(string)
	}
	if envs["EXECD_RUNTIME_INIT"] != "true" {
		t.Fatal("warm pods must wait for execd binding (EXECD_RUNTIME_INIT)")
	}
	req := ctr["resources"].(map[string]any)["requests"].(map[string]any)
	if req["ephemeral-storage"] != "512Mi" {
		t.Fatal("disk request must be small (scheduler reserves it; see validation: disk reservation capped density)")
	}
	if pt["metadata"].(map[string]any)["labels"].(map[string]any)[LabelSandbox] != "true" {
		t.Fatal("pods must carry the sandbox label used by the baseline NetworkPolicy")
	}
}

func TestApplySandboxPolicyAllowNeverOpensPrivateRanges(t *testing.T) {
	c, k := testCell()
	ctx := context.Background()
	if err := c.ApplySandboxPolicy(ctx, "abc", true, nil); err != nil {
		t.Fatal(err)
	}
	np, err := k.NetworkingV1().NetworkPolicies("sb").Get(ctx, "srvm-sb-abc", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if np.Spec.PodSelector.MatchLabels[LabelID] != "abc" {
		t.Fatal("policy must select only this sandbox")
	}
	except := strings.Join(np.Spec.Egress[0].To[0].IPBlock.Except, ",")
	for _, cidr := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16"} {
		if !strings.Contains(except, cidr) {
			t.Fatalf("allow-all must still exclude %s (cluster, metadata)", cidr)
		}
	}
	// deny + no rules → no per-sandbox policy (baseline deny applies)
	if err := c.ApplySandboxPolicy(ctx, "abc", false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := k.NetworkingV1().NetworkPolicies("sb").Get(ctx, "srvm-sb-abc", metav1.GetOptions{}); err == nil {
		t.Fatal("deny-all should remove the per-sandbox allow policy")
	}
}

func TestBaselinePolicyIsolatesSandboxes(t *testing.T) {
	c, k := testCell()
	ctx := context.Background()
	if err := c.EnsureBasePolicies(ctx); err != nil {
		t.Fatal(err)
	}
	np, err := k.NetworkingV1().NetworkPolicies("sb").Get(ctx, "srvm-sandbox-baseline", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(np.Spec.Ingress) != 1 || np.Spec.Ingress[0].From[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "sys" {
		t.Fatal("ingress must be allowed only from the system namespace (no sandbox↔sandbox)")
	}
	if len(np.Spec.Egress) != 2 {
		t.Fatalf("baseline egress must be exactly DNS + object store, got %d rules", len(np.Spec.Egress))
	}
	if err := c.EnsureBasePolicies(ctx); err != nil { // idempotent
		t.Fatal(err)
	}
}

func TestPodTemplateHoldsShutdownForSave(t *testing.T) {
	c, _ := testCell()
	spec := c.podTemplate("img-x", "python:3.12")["spec"].(map[string]any)
	if spec["terminationGracePeriodSeconds"] != int64(90) {
		t.Fatalf("default grace period must be 90s, got %v", spec["terminationGracePeriodSeconds"])
	}
	ctr := spec["containers"].([]any)[0].(map[string]any)
	cmd := ctr["lifecycle"].(map[string]any)["preStop"].(map[string]any)["exec"].(map[string]any)["command"].([]any)
	if cmd[0] != SnapTool || cmd[1] != "prestop" || cmd[2] != GuardMarker || cmd[3] != "85" {
		t.Fatalf("preStop must run srvm-snap prestop with a deadline inside the grace period, got %v", cmd)
	}
	tol := map[string]any{}
	for _, x := range spec["tolerations"].([]any) {
		m := x.(map[string]any)
		tol[m["key"].(string)] = m["tolerationSeconds"]
	}
	if tol["node.kubernetes.io/unreachable"] != int64(30) || tol["node.kubernetes.io/not-ready"] != int64(30) {
		t.Fatalf("pods on a lost node must be evicted after 30s, not the 300s default: %v", tol)
	}
}

func sandboxPod(name, id, ip string, restarts int32, ann map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "sb", Labels: map[string]string{LabelID: id}, Annotations: ann},
		Status: corev1.PodStatus{PodIP: ip, Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{
			{Name: "execd-sidecar-not-ours", RestartCount: 9},
			{Name: "sandbox", RestartCount: restarts, LastTerminationState: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}}},
		}},
	}
}

func TestMarkBoundAndSandboxPods(t *testing.T) {
	c, k := testCell()
	ctx := context.Background()
	_, _ = k.CoreV1().Pods("sb").Create(ctx, sandboxPod("p1", "sb-1", "10.0.0.5", 2, nil), metav1.CreateOptions{})
	pods, err := c.SandboxPods(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p := pods["p1"]; p.BoundRestarts != -1 || p.Restarts != 2 || p.SandboxID != "sb-1" || p.LastExit != "OOMKilled, exit code 137" {
		t.Fatalf("before MarkBound: %+v", p)
	}
	if err := c.MarkBound(ctx, "p1"); err != nil {
		t.Fatal(err)
	}
	got, _ := k.CoreV1().Pods("sb").Get(ctx, "p1", metav1.GetOptions{})
	if got.Annotations[AnnSafeToEvict] != "false" {
		t.Fatal("bound pods must be excluded from autoscaler scale-down")
	}
	pods, _ = c.SandboxPods(ctx)
	if p := pods["p1"]; p.BoundRestarts != 2 {
		t.Fatalf("bound restart count must be recorded, got %d", p.BoundRestarts)
	}
}

func TestWatchTerminatingReportsDeletingPods(t *testing.T) {
	c, k := testCell()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan string, 4)
	go c.WatchTerminating(ctx, func(pod, id string) { got <- pod + "/" + id })
	time.Sleep(100 * time.Millisecond) // let the watch start
	p := sandboxPod("p2", "sb-2", "10.0.0.6", 0, nil)
	_, _ = k.CoreV1().Pods("sb").Create(ctx, p, metav1.CreateOptions{})
	now := metav1.Now()
	p.DeletionTimestamp = &now
	_, _ = k.CoreV1().Pods("sb").Update(ctx, p, metav1.UpdateOptions{})
	select {
	case v := <-got:
		if v != "p2/sb-2" {
			t.Fatalf("got %q", v)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("terminating pod not reported")
	}
}
