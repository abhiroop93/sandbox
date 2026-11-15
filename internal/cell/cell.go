// Package cell is the only srvm component that knows sandboxes are Kubernetes pods (docs/architecture.md D6, D10).
// It manages agent-sandbox SandboxTemplates + SandboxWarmPools (one per image/template), claims warm pods,
// releases them, and owns per-sandbox NetworkPolicies.
package cell

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

var (
	gvrTemplate = schema.GroupVersionResource{Group: "extensions.agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxtemplates"}
	gvrPool     = schema.GroupVersionResource{Group: "extensions.agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxwarmpools"}
	gvrClaim    = schema.GroupVersionResource{Group: "extensions.agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxclaims"}
	gvrSandbox  = schema.GroupVersionResource{Group: "agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxes"}
)

const (
	ExecdPort    = 44772
	LabelManaged = "srvm.io/managed"
	LabelSandbox = "srvm.io/sandbox" // "true" on every srvm sandbox pod (network policy selector)
	LabelID      = "srvm.io/sandbox-id"
	LabelTenant  = "srvm.io/tenant"
	LabelTmpl    = "srvm.io/template"

	// SnapTool and GuardMarker live on the srvm-bin emptyDir: outside /workspace (never snapshotted), and they survive
	// container restarts within the pod.
	SnapTool    = "/opt/srvm-bin/srvm-snap"
	GuardMarker = "/opt/srvm-bin/.armed"

	AnnSafeToEvict   = "cluster-autoscaler.kubernetes.io/safe-to-evict"
	AnnBoundRestarts = "srvm.io/bound-restarts" // sandbox container restartCount when execd was last bound
	AnnTemplateRev   = "srvm.io/template-rev"
	templateRev      = "3" // bump when podTemplate changes so existing templates are updated in place
)

// Config for one cell.
type Config struct {
	Namespace        string // where sandboxes live
	SystemNamespace  string // where srvm + object store run (allowed to reach execd / be reached for snapshots)
	RuntimeClass     string // "gvisor"
	ExecdImage       string // docker.io/opensandbox/execd:release-1.2.0
	HelperImage      string // srvm helper image providing /srvm-snap (workspace snapshot tool)
	DefaultPoolSize  int
	CPU, Memory      string
	DiskRequest      string // ephemeral-storage request (small; see docs/validation.md, disk reservation)
	DiskLimit        string // ephemeral-storage limit (workspace cap)
	ObjectStorePort  int    // egress allowed to object store service in SystemNamespace
	ObjectStoreLabel string // pod label value app=<x> of object store
	ShutdownGrace    int64  // terminationGracePeriodSeconds: time to save the workspace when a pod is shut down
	NodeLossSeconds  int64  // evict pods from a NotReady/unreachable node after this long (k8s default: 300)
}

type Cell struct {
	cfg Config
	dyn dynamic.Interface
	k8s kubernetes.Interface
}

func New(cfg Config, dyn dynamic.Interface, k8s kubernetes.Interface) *Cell {
	if cfg.ShutdownGrace <= 0 {
		cfg.ShutdownGrace = 90
	}
	if cfg.NodeLossSeconds <= 0 {
		cfg.NodeLossSeconds = 30
	}
	return &Cell{cfg: cfg, dyn: dyn, k8s: k8s}
}

// TemplateName derives a stable template/pool name for an image (one warm pool per image).
func TemplateName(image string) string {
	h := sha256.Sum256([]byte(image))
	base := strings.ToLower(image)
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	clean := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}, base)
	if len(clean) > 30 {
		clean = clean[:30]
	}
	return "img-" + strings.Trim(clean, "-") + "-" + hex.EncodeToString(h[:])[:8]
}

// EnsureTemplate creates (idempotently) the SandboxTemplate + SandboxWarmPool for an image.
// Warm pods run execd in "await binding" mode (EXECD_RUNTIME_INIT) so per-sandbox identity, token and env
// are pushed AFTER the claim (keeps the claim on the warm path; see docs/architecture.md D4).
func (c *Cell) EnsureTemplate(ctx context.Context, image string, replicas int) (string, error) {
	name := TemplateName(image)
	ns := c.cfg.Namespace
	cur, err := c.dyn.Resource(gvrTemplate).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		if cur.GetAnnotations()[AnnTemplateRev] == templateRev {
			return name, nil
		}
		// Older srvm wrote this template: bring the pod template up to date. Only pods created from now on get it;
		// existing warm pods keep the old spec until they are used or deleted.
		ann := cur.GetAnnotations()
		if ann == nil {
			ann = map[string]string{}
		}
		ann[AnnTemplateRev] = templateRev
		cur.SetAnnotations(ann)
		if err := unstructured.SetNestedField(cur.Object, c.podTemplate(name, image), "spec", "podTemplate"); err != nil {
			return "", err
		}
		if _, err := c.dyn.Resource(gvrTemplate).Namespace(ns).Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
			return "", fmt.Errorf("update template: %w", err)
		}
		return name, nil
	}
	if !apierrors.IsNotFound(err) {
		return "", err
	}
	tmpl := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "extensions.agents.x-k8s.io/v1beta1", "kind": "SandboxTemplate",
		"metadata": map[string]any{"name": name, "namespace": ns,
			"labels": map[string]any{LabelManaged: "true"}, "annotations": map[string]any{"srvm.io/image": image, AnnTemplateRev: templateRev}},
		"spec": map[string]any{
			"networkPolicyManagement": "Unmanaged", // srvm owns network policy (label-based, see EnsureBasePolicies)
			"podTemplate":             c.podTemplate(name, image),
		},
	}}
	if _, err := c.dyn.Resource(gvrTemplate).Namespace(ns).Create(ctx, tmpl, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", fmt.Errorf("create template: %w", err)
	}
	pool := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "extensions.agents.x-k8s.io/v1beta1", "kind": "SandboxWarmPool",
		"metadata": map[string]any{"name": name, "namespace": ns, "labels": map[string]any{LabelManaged: "true"}},
		"spec":     map[string]any{"replicas": int64(replicas), "sandboxTemplateRef": map[string]any{"name": name}},
	}}
	if _, err := c.dyn.Resource(gvrPool).Namespace(ns).Create(ctx, pool, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", fmt.Errorf("create warm pool: %w", err)
	}
	return name, nil
}

func (c *Cell) podTemplate(tmplName, image string) map[string]any {
	res := map[string]any{
		"requests": map[string]any{"cpu": c.cfg.CPU, "memory": c.cfg.Memory, "ephemeral-storage": c.cfg.DiskRequest},
		"limits":   map[string]any{"cpu": c.cfg.CPU, "memory": c.cfg.Memory, "ephemeral-storage": c.cfg.DiskLimit},
	}
	mounts := []any{
		map[string]any{"name": "opensandbox-bin", "mountPath": "/opt/opensandbox"},
		map[string]any{"name": "srvm-bin", "mountPath": "/opt/srvm-bin"},
		map[string]any{"name": "workspace", "mountPath": "/workspace"},
	}
	return map[string]any{
		"metadata": map[string]any{"labels": map[string]any{LabelSandbox: "true", LabelTmpl: tmplName, LabelManaged: "true"}},
		"spec": map[string]any{
			"runtimeClassName":              c.cfg.RuntimeClass,
			"terminationGracePeriodSeconds": c.cfg.ShutdownGrace,
			// A crashed node keeps reporting its pods Running until they are evicted (default 5 min). Evict sooner so
			// srvm falls back to the last snapshot quickly; sandboxes can't be moved live anyway.
			"tolerations": []any{
				map[string]any{"key": "node.kubernetes.io/not-ready", "operator": "Exists", "effect": "NoExecute", "tolerationSeconds": c.cfg.NodeLossSeconds},
				map[string]any{"key": "node.kubernetes.io/unreachable", "operator": "Exists", "effect": "NoExecute", "tolerationSeconds": c.cfg.NodeLossSeconds},
			},
			"automountServiceAccountToken": false,
			"enableServiceLinks":           false,
			"securityContext": map[string]any{"runAsUser": int64(1000), "runAsGroup": int64(1000), "fsGroup": int64(1000),
				"runAsNonRoot": true, "seccompProfile": map[string]any{"type": "RuntimeDefault"}},
			"volumes": []any{
				map[string]any{"name": "opensandbox-bin", "emptyDir": map[string]any{}},
				map[string]any{"name": "srvm-bin", "emptyDir": map[string]any{}},
				map[string]any{"name": "workspace", "emptyDir": map[string]any{"sizeLimit": c.cfg.DiskLimit}},
			},
			"initContainers": []any{
				map[string]any{"name": "execd-installer", "image": c.cfg.ExecdImage, "imagePullPolicy": "IfNotPresent",
					"command":      []any{"/bin/sh", "-c"},
					"args":         []any{"cp ./execd ./bootstrap.sh /opt/opensandbox/ && chmod 0755 /opt/opensandbox/execd /opt/opensandbox/bootstrap.sh"},
					"volumeMounts": []any{mounts[0]}},
				map[string]any{"name": "srvm-helper-installer", "image": c.cfg.HelperImage, "imagePullPolicy": "IfNotPresent",
					"command":      []any{"/srvm-snap", "install", "/opt/srvm-bin/srvm-snap"},
					"volumeMounts": []any{mounts[1]}},
			},
			"containers": []any{map[string]any{
				"name": "sandbox", "image": image, "imagePullPolicy": "IfNotPresent",
				"command":    []any{"/opt/opensandbox/bootstrap.sh", "sleep", "infinity"},
				"workingDir": "/workspace",
				"env": []any{
					map[string]any{"name": "EXECD", "value": "/opt/opensandbox/execd"},
					map[string]any{"name": "EXECD_RUNTIME_INIT", "value": "true"}, // wait for POST /internal/init after claim
					map[string]any{"name": "HOME", "value": "/workspace"},
				},
				"ports": []any{map[string]any{"name": "execd", "containerPort": int64(ExecdPort)}},
				// On graceful shutdown (drain, eviction, scale-down) hold the pod while srvm saves the workspace.
				// Returns at once unless srvm armed the pod (bound to a sandbox and not yet saved).
				"lifecycle": map[string]any{"preStop": map[string]any{"exec": map[string]any{"command": []any{
					SnapTool, "prestop", GuardMarker, fmt.Sprint(c.cfg.ShutdownGrace - 5)}}}},
				"readinessProbe": map[string]any{"httpGet": map[string]any{"path": "/ping", "port": int64(ExecdPort)}, "periodSeconds": int64(1)},
				"securityContext": map[string]any{"allowPrivilegeEscalation": false,
					"capabilities": map[string]any{"drop": []any{"ALL"}}},
				"resources":    res,
				"volumeMounts": mounts,
			}},
		},
	}
}

// Claim binds a warm pod to the sandbox. Returns claim name; the pod is discovered with WaitReady.
func (c *Cell) Claim(ctx context.Context, template, sandboxID, tenant string, gen int64) (string, error) {
	name := fmt.Sprintf("sb-%s-g%d", sandboxID[:18], gen) // deterministic per generation (D6)
	claim := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "extensions.agents.x-k8s.io/v1beta1", "kind": "SandboxClaim",
		"metadata": map[string]any{"name": name, "namespace": c.cfg.Namespace,
			"labels": map[string]any{LabelManaged: "true", LabelID: sandboxID, LabelTenant: tenant}},
		"spec": map[string]any{
			"warmPoolRef": map[string]any{"name": template},
			// Labels are applied in place on the adopted pod (no cold start) and drive per-sandbox NetworkPolicy.
			"additionalPodMetadata": map[string]any{"labels": map[string]any{LabelID: sandboxID, LabelTenant: tenant}},
			"lifecycle":             map[string]any{"shutdownPolicy": "Delete"},
		},
	}}
	_, err := c.dyn.Resource(gvrClaim).Namespace(c.cfg.Namespace).Create(ctx, claim, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return name, nil
	}
	return name, err
}

// WaitReady waits for the claim to be Ready and returns the backing pod name and IP.
func (c *Cell) WaitReady(ctx context.Context, claim string, timeout time.Duration) (pod, ip string, err error) {
	deadline := time.Now().Add(timeout)
	for {
		o, err := c.dyn.Resource(gvrClaim).Namespace(c.cfg.Namespace).Get(ctx, claim, metav1.GetOptions{})
		if err == nil {
			conds, _, _ := unstructured.NestedSlice(o.Object, "status", "conditions")
			ready := false
			for _, ci := range conds {
				m, ok := ci.(map[string]any)
				if !ok || m["type"] != "Ready" {
					continue
				}
				if m["status"] == "True" {
					ready = true
				} else if reason, _ := m["reason"].(string); strings.HasPrefix(reason, "Invalid") {
					// Terminal claim errors (e.g. InvalidMetadata) never resolve by waiting: fail fast with the cause.
					return "", "", fmt.Errorf("claim %s rejected: %s: %v", claim, reason, m["message"])
				}
			}
			sbName, _, _ := unstructured.NestedString(o.Object, "status", "sandbox", "name")
			ips, _, _ := unstructured.NestedStringSlice(o.Object, "status", "sandbox", "podIPs")
			if ready && sbName != "" && len(ips) > 0 {
				podName, err := c.podFor(ctx, sbName)
				if err != nil {
					return "", "", err
				}
				return podName, ips[0], nil
			}
		} else if !apierrors.IsNotFound(err) {
			return "", "", err
		}
		if time.Now().After(deadline) {
			return "", "", fmt.Errorf("claim %s not ready after %s", claim, timeout)
		}
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (c *Cell) podFor(ctx context.Context, sandboxName string) (string, error) {
	if _, err := c.k8s.CoreV1().Pods(c.cfg.Namespace).Get(ctx, sandboxName, metav1.GetOptions{}); err == nil {
		return sandboxName, nil
	}
	sb, err := c.dyn.Resource(gvrSandbox).Namespace(c.cfg.Namespace).Get(ctx, sandboxName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	sel, _, _ := unstructured.NestedString(sb.Object, "status", "selector")
	pods, err := c.k8s.CoreV1().Pods(c.cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil || len(pods.Items) == 0 {
		return "", fmt.Errorf("no pod for sandbox %s: %v", sandboxName, err)
	}
	return pods.Items[0].Name, nil
}

// Release deletes the claim (agent-sandbox deletes the pod; pods are never reused across sandboxes).
func (c *Cell) Release(ctx context.Context, claim string) error {
	err := c.dyn.Resource(gvrClaim).Namespace(c.cfg.Namespace).Delete(ctx, claim, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// ListClaims returns managed claim names → sandbox id (for reconciliation).
func (c *Cell) ListClaims(ctx context.Context) (map[string]string, error) {
	l, err := c.dyn.Resource(gvrClaim).Namespace(c.cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: LabelManaged + "=true"})
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, o := range l.Items {
		out[o.GetName()] = o.GetLabels()[LabelID]
	}
	return out, nil
}

// ---------------------------------------------------------------- pod health (interruption handling)

// PodInfo is what reconciliation needs to know about a claimed sandbox pod.
type PodInfo struct {
	Name, SandboxID, IP string
	Phase               corev1.PodPhase
	Terminating         bool  // deletionTimestamp set: graceful shutdown in progress
	Restarts            int32 // sandbox container restartCount
	BoundRestarts       int32 // restartCount recorded when execd was bound (-1 = not recorded)
	LastExit            string
}

func podInfo(p *corev1.Pod) PodInfo {
	pi := PodInfo{Name: p.Name, SandboxID: p.Labels[LabelID], IP: p.Status.PodIP, Phase: p.Status.Phase,
		Terminating: p.DeletionTimestamp != nil, BoundRestarts: -1}
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Name == "sandbox" {
			pi.Restarts = cs.RestartCount
			if t := cs.LastTerminationState.Terminated; t != nil {
				pi.LastExit = fmt.Sprintf("%s, exit code %d", t.Reason, t.ExitCode)
			}
		}
	}
	var n int32
	if v, ok := p.Annotations[AnnBoundRestarts]; ok {
		if _, err := fmt.Sscan(v, &n); err == nil {
			pi.BoundRestarts = n
		}
	}
	return pi
}

// SandboxPods returns claimed sandbox pods (those carrying a sandbox id) by pod name. One list call per tick.
func (c *Cell) SandboxPods(ctx context.Context) (map[string]PodInfo, error) {
	l, err := c.k8s.CoreV1().Pods(c.cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: LabelID})
	if err != nil {
		return nil, err
	}
	out := make(map[string]PodInfo, len(l.Items))
	for i := range l.Items {
		out[l.Items[i].Name] = podInfo(&l.Items[i])
	}
	return out, nil
}

// MarkBound records the container restart count execd was bound at and tells the cluster autoscaler not to pick
// this pod's node for scale-down (it would destroy an unsaved workspace).
func (c *Cell) MarkBound(ctx context.Context, pod string) error {
	p, err := c.k8s.CoreV1().Pods(c.cfg.Namespace).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		return err
	}
	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:"false",%q:"%d"}}}`, AnnSafeToEvict, AnnBoundRestarts, podInfo(p).Restarts)
	_, err = c.k8s.CoreV1().Pods(c.cfg.Namespace).Patch(ctx, pod, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	return err
}

// WatchTerminating calls fn for each claimed sandbox pod that starts graceful shutdown, until ctx ends.
// Events can repeat; fn must be idempotent. Reconciliation also checks Terminating, covering missed events.
func (c *Cell) WatchTerminating(ctx context.Context, fn func(pod, sandboxID string)) {
	for ctx.Err() == nil {
		w, err := c.k8s.CoreV1().Pods(c.cfg.Namespace).Watch(ctx, metav1.ListOptions{LabelSelector: LabelID})
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}
		for ev := range w.ResultChan() {
			if p, ok := ev.Object.(*corev1.Pod); ok && p.DeletionTimestamp != nil {
				fn(p.Name, p.Labels[LabelID])
			}
		}
		w.Stop()
	}
}

// ---------------------------------------------------------------- network policy (D8)

// EnsureBasePolicies installs the namespace-wide baseline for srvm sandboxes:
//   - ingress: only from srvm-system to execd (and any port, for the gateway's port proxy); never sandbox↔sandbox
//   - egress: DNS + object store only (everything else is opened per sandbox, see ApplySandboxPolicy)
func (c *Cell) EnsureBasePolicies(ctx context.Context) error {
	tcp, udp := corev1.ProtocolTCP, corev1.ProtocolUDP
	dns := intstr.FromInt32(53)
	os := intstr.FromInt32(int32(c.cfg.ObjectStorePort))
	sysNS := &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": c.cfg.SystemNamespace}}
	np := &netv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "srvm-sandbox-baseline", Namespace: c.cfg.Namespace, Labels: map[string]string{LabelManaged: "true"}},
		Spec: netv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{LabelSandbox: "true"}},
			PolicyTypes: []netv1.PolicyType{netv1.PolicyTypeIngress, netv1.PolicyTypeEgress},
			Ingress:     []netv1.NetworkPolicyIngressRule{{From: []netv1.NetworkPolicyPeer{{NamespaceSelector: sysNS}}}},
			Egress: []netv1.NetworkPolicyEgressRule{
				{To: []netv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{},
					PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}}}},
					Ports: []netv1.NetworkPolicyPort{{Protocol: &udp, Port: &dns}, {Protocol: &tcp, Port: &dns}}},
				{To: []netv1.NetworkPolicyPeer{{NamespaceSelector: sysNS,
					PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": c.cfg.ObjectStoreLabel}}}},
					Ports: []netv1.NetworkPolicyPort{{Protocol: &tcp, Port: &os}}},
			},
		},
	}
	return c.apply(ctx, np)
}

// EgressRule is the OpenSandbox NetworkRule subset srvm supports without an FQDN-aware CNI.
type EgressRule struct {
	Action string // allow (deny rules are implicit: default deny)
	Target string // CIDR or IP. FQDNs require Cilium (rejected by the API layer for now).
	Ports  []int
}

// ApplySandboxPolicy opens egress for one sandbox. defaultAllow=true → internet (non-private ranges) allowed.
func (c *Cell) ApplySandboxPolicy(ctx context.Context, sandboxID string, defaultAllow bool, rules []EgressRule) error {
	var egress []netv1.NetworkPolicyEgressRule
	if defaultAllow {
		egress = append(egress, netv1.NetworkPolicyEgressRule{To: []netv1.NetworkPolicyPeer{{IPBlock: &netv1.IPBlock{
			CIDR: "0.0.0.0/0", Except: []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "100.64.0.0/10"}}}}})
	}
	for _, r := range rules {
		if r.Action != "allow" {
			continue
		}
		cidr := r.Target
		if !strings.Contains(cidr, "/") {
			cidr += "/32"
		}
		rule := netv1.NetworkPolicyEgressRule{To: []netv1.NetworkPolicyPeer{{IPBlock: &netv1.IPBlock{CIDR: cidr}}}}
		for _, p := range r.Ports {
			tcp, port := corev1.ProtocolTCP, intstr.FromInt32(int32(p))
			rule.Ports = append(rule.Ports, netv1.NetworkPolicyPort{Protocol: &tcp, Port: &port})
		}
		egress = append(egress, rule)
	}
	name := "srvm-sb-" + sandboxID
	if len(egress) == 0 {
		return c.DeleteSandboxPolicy(ctx, sandboxID)
	}
	np := &netv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.cfg.Namespace, Labels: map[string]string{LabelManaged: "true", LabelID: sandboxID}},
		Spec: netv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{LabelID: sandboxID}},
			PolicyTypes: []netv1.PolicyType{netv1.PolicyTypeEgress},
			Egress:      egress,
		},
	}
	return c.apply(ctx, np)
}

func (c *Cell) DeleteSandboxPolicy(ctx context.Context, sandboxID string) error {
	err := c.k8s.NetworkingV1().NetworkPolicies(c.cfg.Namespace).Delete(ctx, "srvm-sb-"+sandboxID, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func (c *Cell) apply(ctx context.Context, np *netv1.NetworkPolicy) error {
	api := c.k8s.NetworkingV1().NetworkPolicies(np.Namespace)
	cur, err := api.Get(ctx, np.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = api.Create(ctx, np, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	np.ResourceVersion = cur.ResourceVersion
	_, err = api.Update(ctx, np, metav1.UpdateOptions{})
	return err
}
