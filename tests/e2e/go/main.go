// Go SDK smoke test: the UNMODIFIED OpenSandbox Go SDK against the server.
//
//	SANDBOX_DOMAIN=127.0.0.1:18081 SANDBOX_API_KEY=sandbox_... go run .
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
)

func must(err error, what string) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL %s: %v\n", what, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, sb *opensandbox.Sandbox, cmd string) string {
	ex, err := sb.RunCommand(ctx, cmd, nil)
	must(err, "run "+cmd)
	var b strings.Builder
	for _, m := range ex.Stdout {
		b.WriteString(m.Text)
	}
	return strings.TrimSpace(b.String())
}

func main() {
	ctx := context.Background()
	cfg := opensandbox.ConnectionConfig{Domain: os.Getenv("SANDBOX_DOMAIN"), APIKey: os.Getenv("SANDBOX_API_KEY"),
		UseServerProxy: true, RequestTimeout: 5 * time.Minute}

	t0 := time.Now()
	sb, err := opensandbox.CreateSandbox(ctx, cfg, opensandbox.SandboxCreateOptions{
		Image: "python:3.12-slim", Env: map[string]string{"FROM": "go-sdk"}, ReadyTimeout: 2 * time.Minute})
	must(err, "create")
	fmt.Printf("create+ready: %.2fs id=%s\n", time.Since(t0).Seconds(), sb.ID()[:8])
	fmt.Println("exec:", run(ctx, sb, "python -c 'print(6*7)'; echo $FROM; id -u"))

	must(run2(ctx, sb, "echo persisted-from-go > /workspace/go.txt"), "write")
	must(sb.Pause(ctx), "pause")
	for {
		info, err := sb.GetInfo(ctx)
		must(err, "get info")
		if info.Status.State == opensandbox.StatePaused {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	id := sb.ID()
	_ = sb.Close()
	t1 := time.Now()
	sb, err = opensandbox.ResumeSandbox(ctx, cfg, id, opensandbox.ReadyOptions{Timeout: 2 * time.Minute})
	must(err, "resume")
	fmt.Printf("resume: %.2fs → %s\n", time.Since(t1).Seconds(), run(ctx, sb, "cat /workspace/go.txt; echo $FROM"))
	must(sb.Kill(ctx), "kill")
	fmt.Println("OK: Go SDK create / exec / env / pause / resume / kill against the server")
}

func run2(ctx context.Context, sb *opensandbox.Sandbox, cmd string) error {
	_, err := sb.RunCommand(ctx, cmd, nil)
	return err
}
