// sandbox: server for untrusted-code sandboxes (OpenSandbox-compatible API on kubernetes-sigs/agent-sandbox).
//
//	sandbox serve                                  run the API server + lifecycle loops
//	sandbox admin create-tenant <id> [maxRunning]  operator: create/update a tenant (quota = max running sandboxes)
//	sandbox admin create-key <tenant>              operator: mint an API key (printed once)
//	sandbox admin list-keys [tenant]
//	sandbox admin revoke-key <keyId>
//	sandbox admin usage
//
// Admin commands talk to Postgres directly (run them inside the server pod: kubectl exec deploy/sandbox -- sandbox admin ...).
// The same operations exist over HTTP at /admin/v1/* behind X-SANDBOX-ADMIN-TOKEN.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/abhiroop93/sandbox/internal/api"
	"github.com/abhiroop93/sandbox/internal/cell"
	"github.com/abhiroop93/sandbox/internal/lifecycle"
	"github.com/abhiroop93/sandbox/internal/snapshot"
	"github.com/abhiroop93/sandbox/internal/store"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: sandbox serve | sandbox admin <command>")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch os.Args[1] {
	case "serve":
		if err := serve(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "sandbox:", err)
			os.Exit(1)
		}
	case "admin":
		if err := admin(ctx, os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "sandbox admin:", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, "unknown command", os.Args[1])
		os.Exit(2)
	}
}

func serve(ctx context.Context) error {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	st, err := store.Open(ctx, env("DATABASE_URL", ""))
	if err != nil {
		return err
	}
	st.MaxImages = envInt("MAX_IMAGES_PER_TENANT", 10)
	defer st.Close()

	rc, err := rest.InClusterConfig()
	if err != nil {
		if rc, err = clientcmd.BuildConfigFromFlags("", env("KUBECONFIG", os.Getenv("HOME")+"/.kube/config")); err != nil {
			return err
		}
	}
	rc.QPS, rc.Burst = 50, 100
	rc.Timeout = time.Duration(envInt("KUBERNETES_REQUEST_TIMEOUT_SECONDS", 30)) * time.Second
	dyn, err := dynamic.NewForConfig(rc)
	if err != nil {
		return err
	}
	k8s, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return err
	}
	c := cell.New(cell.Config{
		Namespace:        env("SANDBOX_NAMESPACE", "sandboxes"),
		SystemNamespace:  env("SYSTEM_NAMESPACE", "sandbox-system"),
		RuntimeClass:     env("RUNTIME_CLASS", "gvisor"),
		ExecdImage:       env("EXECD_IMAGE", "docker.io/opensandbox/execd:release-1.2.0"),
		HelperImage:      env("HELPER_IMAGE", "sandbox:dev"),
		DefaultPoolSize:  envInt("POOL_SIZE", 2),
		CPU:              env("SANDBOX_CPU", "1"),
		Memory:           env("SANDBOX_MEMORY", "2Gi"),
		DiskRequest:      env("SANDBOX_DISK_REQUEST", "512Mi"),
		DiskLimit:        env("SANDBOX_DISK_LIMIT", "6Gi"),
		ObjectStorePort:  envInt("OBJECT_STORE_PORT", 8080),
		ObjectStoreLabel: env("OBJECT_STORE_APP_LABEL", "s3"),
		ShutdownGrace:    int64(envInt("SHUTDOWN_GRACE_SECONDS", 90)),
		NodeLossSeconds:  int64(envInt("NODE_LOSS_SECONDS", 30)),
		MaxTemplates:     envInt("MAX_IMAGES", 50),
	}, dyn, k8s)
	if err := c.EnsureBasePolicies(ctx); err != nil {
		return fmt.Errorf("base network policies: %w", err)
	}
	snap, err := snapshot.New(env("S3_ENDPOINT", "s3.sandbox-system.svc.cluster.local:8080"), env("S3_ACCESS_KEY", ""),
		env("S3_SECRET_KEY", ""), env("S3_BUCKET", "sandbox-snapshots"), env("S3_SECURE", "false") == "true")
	if err != nil {
		return err
	}
	lc := lifecycle.New(st, c, snap, envInt("POOL_SIZE", 2), env("DEFAULT_EGRESS", "deny"), log)
	lc.ShutdownGrace = time.Duration(envInt("SHUTDOWN_GRACE_SECONDS", 90)) * time.Second
	lc.WorkerCount = envInt("JOB_WORKERS", 4)
	lc.AutosaveInterval = time.Duration(envInt("AUTOSAVE_INTERVAL_SECONDS", 0)) * time.Second
	lc.ReconcileTimeout = time.Duration(envInt("RECONCILE_TIMEOUT_SECONDS", 30)) * time.Second
	lc.RecoverInterrupted(ctx) // reconstruct jobs only for rows written by pre-queue versions
	go lc.Loop(ctx)

	srv := &http.Server{Addr: env("LISTEN", ":8080"),
		Handler:           api.New(st, lc, env("ADMIN_TOKEN", ""), env("PUBLIC_ENDPOINT", ""), log).Handler(),
		ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Info("listening", "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func admin(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("commands: create-tenant <id> [maxRunning] | create-key <tenant> | list-keys [tenant] | revoke-key <keyId> | usage")
	}
	st, err := store.Open(ctx, env("DATABASE_URL", ""))
	if err != nil {
		return err
	}
	defer st.Close()
	out := json.NewEncoder(os.Stdout)
	out.SetIndent("", "  ")
	need := func(n int) error {
		if len(args) < n {
			return fmt.Errorf("missing argument for %s", args[0])
		}
		return nil
	}
	switch args[0] {
	case "create-tenant":
		if err := need(2); err != nil {
			return err
		}
		max := 50
		if len(args) > 2 {
			if max, err = strconv.Atoi(args[2]); err != nil {
				return err
			}
		}
		if err := st.CreateTenant(ctx, args[1], max); err != nil {
			return err
		}
		return out.Encode(map[string]any{"tenant": args[1], "maxRunning": max})
	case "create-key":
		if err := need(2); err != nil {
			return err
		}
		id, key, err := st.CreateKey(ctx, args[1])
		if err != nil {
			return err
		}
		return out.Encode(map[string]string{"keyId": id, "apiKey": key, "tenant": args[1], "note": "store this key now; it cannot be retrieved again"})
	case "list-keys":
		t := ""
		if len(args) > 1 {
			t = args[1]
		}
		keys, err := st.ListKeys(ctx, t)
		if err != nil {
			return err
		}
		return out.Encode(keys)
	case "revoke-key":
		if err := need(2); err != nil {
			return err
		}
		if err := st.RevokeKey(ctx, args[1]); err != nil {
			return err
		}
		return out.Encode(map[string]string{"revoked": args[1]})
	case "usage":
		u, err := st.Usage(ctx)
		if err != nil {
			return err
		}
		return out.Encode(u)
	}
	return fmt.Errorf("unknown admin command %q", args[0])
}
