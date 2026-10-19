//go:build !windows

package sessionproxy

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// helperEnv makes the test binary the stdio MCP server below.
const helperEnv = "SESSIONPROXY_STDIO_HELPER"

const localSecret = "local-secret-9876543210"

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		stdioServer()
		return
	}
	os.Exit(m.Run())
}

// stdioServer is a tiny MCP server on newline-delimited JSON-RPC: each
// request answered on its own goroutine, so a slow one is answered after
// a quick one sent later.
func stdioServer() {
	var mu sync.Mutex
	out := json.NewEncoder(os.Stdout)
	send := func(v any) {
		mu.Lock()
		defer mu.Unlock()
		_ = out.Encode(v)
	}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &msg) != nil || msg.ID == nil {
			continue
		}
		go func() {
			result := func(v any) { send(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": v}) }
			text := func(s string) { result(map[string]any{"content": []any{map[string]any{"type": "text", "text": s}}}) }
			switch msg.Method {
			case "initialize":
				result(map[string]any{"protocolVersion": "2025-06-18", "serverInfo": map[string]any{"name": "helper"}, "capabilities": map[string]any{"tools": map[string]any{}}})
			case "tools/list":
				result(map[string]any{"tools": []any{map[string]any{"name": "echo"}, map[string]any{"name": "secret_hash"}}})
			case "tools/call":
				switch msg.Params.Name {
				case "echo":
					if ms, ok := msg.Params.Arguments["sleep_ms"].(float64); ok {
						time.Sleep(time.Duration(ms) * time.Millisecond)
					}
					text(fmt.Sprint(msg.Params.Arguments["text"]))
				case "secret_hash":
					sum := sha256.Sum256([]byte(os.Getenv("LOCAL_SECRET")))
					text(hex.EncodeToString(sum[:]))
				case "leak":
					text("the secret is " + os.Getenv("LOCAL_SECRET"))
				case "stderr":
					fmt.Fprintf(os.Stderr, "auth with %s failed\n", os.Getenv("LOCAL_SECRET"))
					text("logged")
				case "pid":
					text(fmt.Sprint(os.Getpid()))
				case "notify":
					send(map[string]any{"jsonrpc": "2.0", "method": "notifications/tools/list_changed"})
					text("notified")
				case "exit":
					os.Exit(3)
				}
			}
		}()
	}
}

func helperLocal(t *testing.T) Local {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Local{Command: exe, Args: []string{"-test.run=^$"}, Env: map[string]string{helperEnv: "1"}, Secrets: map[string]string{"LOCAL_SECRET": localSecret}}
}

func call(id int, tool string, args map[string]any) string {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": args}})
	return string(b)
}

// toolText is a tools/call answer's first text and its id.
func toolText(t *testing.T, body string) (string, json.RawMessage) {
	t.Helper()
	var r struct {
		ID     json.RawMessage `json:"id"`
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct{ Message string } `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("answer %q: %v", body, err)
	}
	if r.Error != nil || len(r.Result.Content) == 0 {
		t.Fatalf("answer %q has no text", body)
	}
	return r.Result.Content[0].Text, r.ID
}

func TestProxyLocalAnswersToolsListAndCall(t *testing.T) {
	p := New(map[string]Upstream{"local": helperLocal(t)})
	c, _ := listen(t, p)

	resp, body := post(t, c, "local", nil, initialize)
	if resp.StatusCode != 200 || !strings.Contains(body, `"helper"`) {
		t.Fatalf("initialize: %d %s", resp.StatusCode, body)
	}
	resp, _ = post(t, c, "local", nil, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("notification: status %d, want 202", resp.StatusCode)
	}
	resp, body = post(t, c, "local", nil, `{"jsonrpc":"2.0","id":"list-1","method":"tools/list"}`)
	if resp.StatusCode != 200 || !strings.Contains(body, `"secret_hash"`) || !strings.Contains(body, `"id":"list-1"`) {
		t.Fatalf("tools/list: %d %s", resp.StatusCode, body)
	}
	_, body = post(t, c, "local", nil, call(5, "echo", map[string]any{"text": "hello"}))
	if got, id := toolText(t, body); got != "hello" || string(id) != "5" {
		t.Fatalf("tools/call echo = %q id %s", got, id)
	}
}

func TestProxyLocalChildSeesItsSecretInEnv(t *testing.T) {
	p := New(map[string]Upstream{"local": helperLocal(t)})
	c, _ := listen(t, p)
	if _, ok := os.LookupEnv("LOCAL_SECRET"); ok {
		t.Fatal("LOCAL_SECRET is in the proxy's own environment")
	}

	_, body := post(t, c, "local", nil, call(1, "secret_hash", nil))
	sum := sha256.Sum256([]byte(localSecret))
	if got, _ := toolText(t, body); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("child's LOCAL_SECRET hashes to %s, want the secret's", got)
	}
	// What the child echoes of it is masked on the way back.
	_, body = post(t, c, "local", nil, call(2, "leak", nil))
	if strings.Contains(body, localSecret) || !strings.Contains(body, "[redacted]") {
		t.Fatalf("leaked secret not masked: %s", body)
	}
}

func TestProxyLocalConcurrentCallersGetTheirOwnAnswer(t *testing.T) {
	p := New(map[string]Upstream{"local": helperLocal(t)})
	c, _ := listen(t, p)
	post(t, c, "local", nil, initialize)

	// Every caller uses id 7; the slow ones are answered last.
	const n = 6
	var wg sync.WaitGroup
	got := make([]string, n)
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, body := post(t, c, "local", nil, call(7, "echo", map[string]any{"text": fmt.Sprint("caller-", i), "sleep_ms": (n - i) * 40}))
			text, id := toolText(t, body)
			got[i], ids[i] = text, string(id)
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if want := fmt.Sprint("caller-", i); got[i] != want || ids[i] != "7" {
			t.Errorf("caller %d got %q id %s, want %q id 7", i, got[i], ids[i], want)
		}
	}
}

func pidOf(t *testing.T, c *http.Client) int {
	t.Helper()
	_, body := post(t, c, "local", nil, call(1, "pid", nil))
	text, _ := toolText(t, body)
	var pid int
	if _, err := fmt.Sscan(text, &pid); err != nil || pid <= 0 {
		t.Fatalf("pid %q", text)
	}
	return pid
}

func alive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func TestProxyLocalCloseLeavesNoChildRunning(t *testing.T) {
	p := New(map[string]Upstream{"local": helperLocal(t)})
	c, _ := listen(t, p)
	pid := pidOf(t, c)
	if !alive(pid) {
		t.Fatalf("child %d not running before Close", pid)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if alive(pid) {
		t.Fatalf("child %d still running after Close", pid)
	}
}

func TestProxyLocalStderrIsRedactedInTheLog(t *testing.T) {
	var logs logBuf
	p := New(map[string]Upstream{"local": helperLocal(t)})
	p.Logf = logs.logf
	c, _ := listen(t, p)
	post(t, c, "local", nil, call(1, "stderr", nil))
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), "stderr: auth with") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	got := logs.String()
	if !strings.Contains(got, "local: stderr: auth with [redacted] failed") {
		t.Fatalf("log has no redacted stderr line:\n%s", got)
	}
	if strings.Contains(got, localSecret) {
		t.Fatalf("log carries the secret:\n%s", got)
	}
}

func TestProxyLocalRestartsOnceThenRefuses(t *testing.T) {
	var logs logBuf
	p := New(map[string]Upstream{"local": helperLocal(t)})
	p.Logf = logs.logf
	c, _ := listen(t, p)
	post(t, c, "local", nil, initialize)
	first := pidOf(t, c)

	_, body := post(t, c, "local", nil, call(2, "exit", nil))
	if !strings.Contains(body, "the server exited") || !strings.Contains(body, `"id":2`) {
		t.Fatalf("exit answer: %s", body)
	}
	second := pidOf(t, c)
	if second == first {
		t.Fatalf("not restarted: pid %d both times", first)
	}
	if !strings.Contains(logs.String(), "restarting after exit") {
		t.Errorf("no restart line:\n%s", logs.String())
	}

	post(t, c, "local", nil, call(3, "exit", nil))
	resp, body := post(t, c, "local", nil, call(4, "pid", nil))
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(body, "restarted once already") {
		t.Fatalf("third start: %d %s", resp.StatusCode, body)
	}
}

func TestProxyLocalServerMessagesGoOutOnTheGetStream(t *testing.T) {
	p := New(map[string]Upstream{"local": helperLocal(t)})
	c, _ := listen(t, p)
	post(t, c, "local", nil, initialize)

	req, _ := http.NewRequest(http.MethodGet, "http://proxy"+Prefix+"local", nil)
	req.Header.Set("Accept", "text/event-stream")
	stream, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	if ct := stream.Header.Get("Content-Type"); stream.StatusCode != 200 || !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("GET: %d %s", stream.StatusCode, ct)
	}
	lines := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(stream.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	_, body := post(t, c, "local", nil, call(9, "notify", nil))
	if text, _ := toolText(t, body); text != "notified" {
		t.Fatalf("notify answered %q", text)
	}
	timeout := time.After(5 * time.Second)
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatal("stream ended before the notification")
			}
			if strings.HasPrefix(l, "data: ") && strings.Contains(l, "notifications/tools/list_changed") {
				return
			}
		case <-timeout:
			t.Fatal("no notification on the GET stream")
		}
	}
}
