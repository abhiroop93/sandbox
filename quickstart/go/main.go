// srvm quickstart (Go): create a sandbox, run a few commands, use files, clean up.
//
//	export SRVM_DOMAIN=127.0.0.1:18081   # srvm address (host:port, no scheme)
//	export SRVM_API_KEY=srvm_...         # from your platform team / `srvm admin create-key`
//	go run .
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
)

func main() {
	domain, key := os.Getenv("SRVM_DOMAIN"), os.Getenv("SRVM_API_KEY")
	if domain == "" || key == "" {
		log.Fatal("set SRVM_DOMAIN and SRVM_API_KEY (see README.md)")
	}
	ctx := context.Background()
	cfg := opensandbox.ConnectionConfig{Domain: domain, APIKey: key, UseServerProxy: true, RequestTimeout: 5 * time.Minute}

	t0 := time.Now()
	sb, err := opensandbox.CreateSandbox(ctx, cfg, opensandbox.SandboxCreateOptions{
		Image:        "python:3.12-slim",
		Env:          map[string]string{"GREETING": "hello from srvm"},
		ReadyTimeout: 2 * time.Minute,
	})
	if err != nil {
		log.Fatalf("create: %v", err)
	}
	fmt.Printf("sandbox %s ready in %.2fs\n\n", sb.ID(), time.Since(t0).Seconds())
	defer func() {
		if err := sb.Kill(ctx); err != nil {
			log.Printf("kill: %v", err)
		}
		fmt.Printf("\nsandbox %s deleted\n", sb.ID())
	}()

	sh := func(cmd string) string {
		ex, err := sb.RunCommand(ctx, cmd, nil)
		if err != nil {
			log.Fatalf("run %q: %v", cmd, err)
		}
		var b strings.Builder
		for _, m := range ex.Stdout {
			b.WriteString(m.Text)
		}
		return strings.TrimSpace(b.String())
	}

	fmt.Println("uid         :", sh("id -u"))
	fmt.Println("kernel      :", sh("head -c 45 /proc/version"))
	fmt.Println("python      :", sh("python --version"))
	fmt.Println("env         :", sh("echo $GREETING"))
	fmt.Println("cwd         :", sh("pwd"))
	sh(`cat > /workspace/fib.py <<'PY'
import json
a, b = 0, 1
seq = []
for _ in range(15):
    seq.append(a); a, b = b, a + b
json.dump(seq, open('/workspace/fib.json', 'w'))
print('wrote', len(seq), 'numbers')
PY`)
	fmt.Println("run script  :", sh("python /workspace/fib.py"))
	fmt.Println("read file   :", sh("cat /workspace/fib.json"))
	fmt.Println("ls          :", sh(`ls /workspace | tr '\n' ' '`))
}
