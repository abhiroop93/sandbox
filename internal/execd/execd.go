// Package execd is a minimal client for OpenSandbox's execd (spec execd-api 1.0.0) as used by the server itself:
// binding a claimed warm pod (POST /internal/init) and running internal commands (POST /command, SSE).
// User traffic never goes through this client; it is reverse-proxied by internal/api.
package execd

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const Port = 44772
const TokenHeader = "X-EXECD-ACCESS-TOKEN"

var httpc = &http.Client{Timeout: 0} // streaming commands may run long; callers pass contexts

func base(ip string) string { return fmt.Sprintf("http://%s:%d", ip, Port) }

// NewToken returns a random per-generation execd access token (kept server-side, injected by the proxy).
func NewToken() string { b := make([]byte, 24); _, _ = rand.Read(b); return hex.EncodeToString(b) }

func hash(token string) string {
	h := sha256.Sum256([]byte(token))
	return "sha256:" + hex.EncodeToString(h[:])
}

// Binding is execd's RuntimeBinding (components/execd/pkg/web/model/init.go).
type Binding struct {
	SandboxID        string            `json:"sandboxId"`
	Generation       int64             `json:"generation"`
	EntrypointPolicy string            `json:"entrypointPolicy,omitempty"`
	AccessTokenHash  string            `json:"accessTokenHash"`
	Envs             map[string]string `json:"envs,omitempty"`
	Telemetry        map[string]any    `json:"telemetry,omitempty"`
}

// Bind applies identity, token and env to a freshly claimed warm pod and waits until execd reports ready.
func Bind(ctx context.Context, ip, sandboxID string, gen int64, token string, envs map[string]string, tenant string) error {
	b := Binding{SandboxID: sandboxID, Generation: gen, EntrypointPolicy: "keep", AccessTokenHash: hash(token), Envs: envs,
		Telemetry: map[string]any{"attributes": map[string]string{"tenant": tenant}}}
	body, _ := json.Marshal(b)
	deadline := time.Now().Add(30 * time.Second)
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base(ip)+"/internal/init", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := httpc.Do(req)
		if err == nil {
			msg, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
			if resp.StatusCode == http.StatusBadRequest {
				return fmt.Errorf("execd init rejected: %s", msg)
			}
			err = fmt.Errorf("execd init: %s: %s", resp.Status, msg)
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
	for time.Now().Before(deadline) { // /ready → 200 once bound
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base(ip)+"/ready", nil)
		if resp, err := httpc.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("execd not ready after binding")
}

// Result of an internal command.
type Result struct {
	Stdout, Stderr string
	ExitCode       int
	Error          string
}

// Run executes argv inside the sandbox via execd and collects the SSE output.
func Run(ctx context.Context, ip, token string, argv []string, cwd string, timeout time.Duration) (*Result, error) {
	reqBody := map[string]any{"argv": argv}
	if cwd != "" {
		reqBody["cwd"] = cwd
	}
	if timeout > 0 {
		reqBody["timeout"] = timeout.Milliseconds()
	}
	body, _ := json.Marshal(reqBody)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base(ip)+"/command", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set(TokenHeader, token)
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("execd /command: %s: %s", resp.Status, msg)
	}
	res := &Result{ExitCode: -1}
	var out, errb strings.Builder
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		line = strings.TrimPrefix(line, "data:")
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var ev struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			ExitCode *int   `json:"exit_code"`
			Error    *struct {
				Ename    string `json:"ename"`
				Evalue   string `json:"evalue"`
				ExitCode *int   `json:"exit_code"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "stdout":
			out.WriteString(ev.Text)
		case "stderr":
			errb.WriteString(ev.Text)
		case "error":
			if ev.Error != nil {
				res.Error = ev.Error.Ename + ": " + ev.Error.Evalue
				if ev.Error.ExitCode != nil {
					res.ExitCode = *ev.Error.ExitCode
				} else {
					res.ExitCode = 1
				}
			}
		case "execution_complete":
			if ev.ExitCode != nil {
				res.ExitCode = *ev.ExitCode
			} else if res.ExitCode == -1 {
				res.ExitCode = 0
			}
		}
	}
	res.Stdout, res.Stderr = out.String(), errb.String()
	return res, sc.Err()
}
