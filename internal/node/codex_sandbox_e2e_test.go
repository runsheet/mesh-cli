//go:build !windows

package node

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/harness"
	"projectmesh.io/mesh-cli/internal/sandbox"
)

// codexE2ECredential is the issued Codex credential the e2e runs on, read
// from the environment scripts/e2e-sandbox.sh passes through: a ChatGPT
// login's auth.json (MESH_E2E_CODEX_AUTH, a path — the shape of the
// platform's "codex - chatgpt pro" oauth_token) or an OpenAI API key
// (MESH_E2E_CODEX_API_KEY). Neither is a skip that says so.
func codexE2ECredential(t *testing.T) (gateway.Credential, map[string]any) {
	t.Helper()
	if path := os.Getenv("MESH_E2E_CODEX_AUTH"); path != "" {
		login, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("MESH_E2E_CODEX_AUTH: %v", err)
		}
		cred := gateway.Credential{ID: "oa-e2e", Label: "codex-e2e", Kind: AccountSubscription, Adapter: AdapterCodex, SecretVersion: 1, Secret: string(login)}
		return cred, map[string]any{"id": cred.ID, "label": cred.Label, "vendor": "openai", "kind": "oauth_token", "serves": []any{"codex"}, "secret": cred.Secret, "secretVersion": float64(1)}
	}
	if key := os.Getenv("MESH_E2E_CODEX_API_KEY"); key != "" {
		cred := gateway.Credential{ID: "oa-e2e", Label: "codex-e2e", Kind: AccountAPI, Adapter: AdapterCodex, SecretVersion: 1, Secret: key, CapDailyUSD: 5}
		return cred, map[string]any{"id": cred.ID, "label": cred.Label, "vendor": "openai", "kind": "api_key", "serves": []any{"codex"}, "secret": key, "secretVersion": float64(1), "capUnit": "dollars", "dailyCap": 5.0}
	}
	t.Skip("no Codex credential given: set MESH_E2E_CODEX_AUTH=<a ChatGPT login's auth.json> or MESH_E2E_CODEX_API_KEY=<an OpenAI key> to run a Codex evaluation in a pod")
	return gateway.Credential{}, nil
}

// TestCodexEvaluationInAPodE2E is goal codex-sessions-run-in-the-sandbox's
// e2e (item 5): a runsheet item's evaluation chosen onto Codex by an
// issued Codex credential with headroom runs on podman in its own pod
// from the node image — the image's codex, preflighted in that pod
// (Codex.Preflight: the sandbox probe under the session's own HOME and
// env) and then `codex exec --json --sandbox workspace-write` — on
// runsheet's own test workflow fixture (its Postgres sidecar beside it).
// It passes when evaluateWorkItem records a verdict (not a no-verdict),
// its usage names adapter codex and a gpt- model, Claude never ran, and
// afterwards podman holds no pod or container of the session and no
// session HOME is left. It needs podman, the node image
// (MESH_SANDBOX_IMAGE, else mesh-node at dev) and a Codex credential, so
// it runs only with MESH_SANDBOX_E2E=1 and one of codexE2ECredential's.
func TestCodexEvaluationInAPodE2E(t *testing.T) {
	if os.Getenv("MESH_SANDBOX_E2E") != "1" {
		t.Skip("MESH_SANDBOX_E2E=1 runs real pods")
	}
	cred, raw := codexE2ECredential(t)
	image := os.Getenv("MESH_SANDBOX_IMAGE")
	if image == "" {
		image = sandbox.DefaultImage("dev")
	}
	rt := &sandbox.Podman{}
	if _, err := rt.Version(context.Background()); err != nil {
		t.Fatalf("podman: %v", err)
	}
	slug, home := codexPodEvaluation(t, rt, image, cred, raw)
	assertNoPods(t, "mesh-"+slug+"-")
	t.Logf("podman: no pod or container named mesh-%s-…; HOME root %s empty", slug, home)
}

// The e2e's own path over the fake runtime, in every go test: the same
// evaluator, credential and checks as TestCodexEvaluationInAPodE2E, the
// node image's codex answered by codexPodRuntime, and the pod removed.
func TestCodexEvaluationInAPodOverTheFake(t *testing.T) {
	cred := codexSub()
	raw := map[string]any{"id": cred.ID, "label": cred.Label, "vendor": "openai", "kind": "oauth_token", "serves": []any{"codex"}, "secret": cred.Secret, "secretVersion": float64(1)}
	rt := codexPodRuntime(false, "README.md says what the fixture is for.\nVERDICT: PASS")
	slug, _ := codexPodEvaluation(t, rt, "ghcr.io/runsheet/mesh-node:v1", cred, raw)
	if len(rt.Pods) != 1 || !slices.Contains(rt.Removed, rt.Pods[0].Name) || !strings.HasPrefix(rt.Pods[0].Name, "mesh-"+slug+"-") {
		t.Fatalf("pods %+v, removed %v", rt.Pods, rt.Removed)
	}
	var cmds []string
	for _, a := range rt.Agents {
		cmds = append(cmds, strings.Join(a.Command[:min(2, len(a.Command))], " "))
	}
	if !slices.Equal(cmds, []string{"codex sandbox", "codex exec"}) {
		t.Fatalf("containers %v: want the preflight's sandbox probe, then the session", cmds)
	}
}

// codexPodEvaluation evaluates item 1 of a fresh fake gateway on rt with
// a sandboxed node's evaluator — the Codex harness in the session's pod,
// its Preflight the evaluator's, cred the one Codex account issued and
// known, a Claude harness that must not run — in a checkout of runsheet's
// test workflow fixture (its Postgres sidecar beside it), and checks what
// the e2e's acceptance names: a verdict recorded, its usage naming adapter
// codex and a gpt- model, no secret in the node's log, and no session
// HOME left. It returns the goal's slug, which names the pod, and the
// HOME root.
func codexPodEvaluation(t *testing.T, rt sandbox.Runtime, image string, cred gateway.Credential, raw map[string]any) (slug, homeRoot string) {
	t.Helper()
	// the refreshed login is written back under the user cache dir: keep
	// it in the test's (HOME is left alone: podman finds its store by it)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	slug = "e2e-codex-" + time.Now().Format("150405")
	sb := &sandbox.Sandbox{Runtime: rt, Config: SandboxConfig{Image: image}.Settings("dev"), TempDir: t.TempDir()}
	sb.Config.ServiceTimeout = 3 * time.Minute

	f, cfg := evalFake(t)
	f.Goals[goalID]["slug"] = slug
	f.Goals[goalID]["acceptance"] = "README.md says what the fixture is for"
	f.WorkItems[item1]["branch"] = "mesh/" + slug + "-1"
	f.SessionCredentials[item1] = []map[string]any{claudeCredential, raw}

	gw := gateway.New(f.URL(), "rs_test")
	logs := &syncBuf{}
	logger := log.New(&teeLog{t: t, buf: logs}, "", 0)
	limits, known := &Limits{}, &KnownAccounts{}
	if err := known.Note(IssuedAccount(cred), time.Now()); err != nil {
		t.Fatal(err)
	}
	loop := &Loop{Known: known, Limits: limits}
	ws := &fakeWS{root: t.TempDir()}
	codex := &harness.Codex{Sandbox: sb, Stderr: os.Stderr, WorkRoot: ws.root}
	claude := &fakeHarness{text: "VERDICT: FAIL"}
	ev := &Evaluator{GW: gw, Harness: claude, WS: ws, Scopes: scopesOf(cfg), WorkRoot: ws.root, MaxBudgetUSD: 2, Model: "sonnet",
		Sandbox: sb, Declines: &Declines{}, Timeout: 10 * time.Minute,
		Codex: ObserveLimits(codex, limits), CodexPreflight: codex.Preflight, CodexModel: CodexConfig{}.CodexModel,
		CodexAccounts: loop.CodexCandidates, Waits: &OncePer{}, NoVerdict: &OncePer{}, Limits: limits,
		Thresholds:  func() LimitsConfig { return LimitsConfig{} },
		Credentials: &Credentials{GW: gw, Limits: limits, Known: known, Log: logger}, Log: logger}

	item, err := gw.WorkItem(ctx, item1)
	if err != nil {
		t.Fatal(err)
	}
	dir := EvalWorkspaceSpec(ws.root, slug, 1, "", "", "").Dir()
	copyFixture(t, fixtureRunsheetPostgres, dir)
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# runsheet-postgres\n\nThe fixture for mesh-cli's sandbox e2e: runsheet's test workflow, whose services: is Postgres.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := ev.Evaluate(ctx, *item); err != nil {
		t.Fatalf("evaluate: %v\n%s", err, logs)
	}
	if len(claude.jobs) != 0 {
		t.Fatalf("the evaluation ran on Claude, not Codex:\n%s", logs)
	}
	// the session handed its verdict to the product's tool; the node
	// closed the job with its notes tail (runsheet ADR 077)
	if n := nodeVerdicts(f); n != 0 {
		t.Fatalf("the node handed in a verdict of its own: %d\n%s", n, logs)
	}
	in := evaluationClosed(t, f)
	notes, _ := in["notes"].(string)
	if !strings.Contains(notes, "(evaluated on Codex") {
		t.Fatalf("notes tail %q does not name Codex\n%s", notes, logs)
	}
	t.Logf("evaluation closed; notes tail: %s", firstLine(notes))

	u := usageOf(t, f)
	rec, _ := json.Marshal(u)
	t.Logf("usage record: %s", rec)
	model, _ := u["model"].(string)
	if u["adapter"] != "codex" || !strings.HasPrefix(model, "gpt-") || u["credentialId"] != cred.ID {
		t.Fatalf("usage names adapter %v, model %q, credential %v; want codex, gpt-…, %s", u["adapter"], model, u["credentialId"], cred.ID)
	}
	if strings.Contains(logs.String(), cred.Secret) {
		t.Fatal("the node's log carries the credential's secret")
	}
	if left, _ := os.ReadDir(sb.TempDir); len(left) != 0 {
		t.Fatalf("session HOMEs left: %v", left)
	}
	return slug, sb.TempDir
}

// teeLog writes the node's log to the test's output as well as its
// buffer, so the script's run shows the session's own lines.
type teeLog struct {
	t   *testing.T
	buf *syncBuf
}

func (l *teeLog) Write(p []byte) (int, error) {
	l.t.Log(strings.TrimRight(string(p), "\n"))
	return l.buf.Write(p)
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
