package tripline

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/getsentry/sentry-go"
)

// The planted secrets are put together at run time so no file of the
// repo carries a token's shape whole.
var (
	ghToken     = "gh" + "p_" + "Q7xk2LmP9vR4sT1uW8yZ3aB6cD0eF5gH2jK4"
	ghsToken    = "gh" + "s_" + "N3bV7cX1zL5kJ9hG2fD6sA0pO4iU8yT3rE7w"
	patToken    = "github" + "_pat_" + "11ABCDEFG0aBcDeFgHiJkL_MnOpQrStUvWxYz0123456789AbCdEf"
	antKey      = "sk-" + "ant-" + "api03-Zq8Wv2Xr5Ty9Ui1Op4As7Df0Gh3Jk6Lz"
	openaiKey   = "sk-" + "proj-" + "Hy6Tg2Rf8Ed4Ws0Qa9Zx3Cv7Bn1Mk5Lp"
	consoleTok  = "rs" + "_live_" + "8fJ2kL9mN4pQ7rS1tV6wX3yZ0aB5cD"
	awsKey      = "AKIA" + "IOSFODNN7QWERTYU"
	jwt         = "eyJ" + "hbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9" + ".eyJ" + "zdWIiOiJtYWNoaW5lOm0xIiwib3JnIjoicnMifQ" + ".c2lnbmF0dXJlLWJ5dGVzLWhlcmUtOTk4OA"
	refreshTok  = "Lk7Rf" + "Tq2Wm9Xz4Pn8Bv3Cs6Dh1Gj5Ka0Ye"
	jobSecret   = "job-secret-" + "x9Q2w7E4r1T8y5U3i6O0"
	envSecret   = "envval-" + "Mz3Nx8Bc2Vb7Lk4Jh9Gf"
	operatorHom = "/home/operator"
)

func scrubbed(t *testing.T, s *Scrubber, ev *sentry.Event) string {
	t.Helper()
	out := s.Scrub(ev, &sentry.EventHint{})
	if out == nil {
		t.Fatal("Scrub dropped the event")
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func TestScrubberAdversarial(t *testing.T) {
	tokenFile := `{"kind":"machine","accessToken":"` + jwt + `","refreshToken":"` + refreshTok + `","baseUrl":"https://api.runsheet.dev","machineId":"m1"}`
	s := New(Options{
		Node:    "dl360",
		Home:    operatorHom,
		Secrets: append([]string{jobSecret}, TokenFileSecrets([]byte(tokenFile))...),
		Env:     []string{"MESH_TOOL_KEY=" + envSecret},
	})

	half := len(jobSecret) / 2
	cases := []struct {
		name    string
		event   func() *sentry.Event
		planted []string
		keep    []string
	}{
		{
			name: "secret split across message and breadcrumb",
			event: func() *sentry.Event {
				return &sentry.Event{
					Message: "infrastructure: harness unavailable: env was " + jobSecret[:half],
					Breadcrumbs: []*sentry.Breadcrumb{
						{Category: "session", Message: jobSecret[half:] + " (continued)"},
						{Category: "log", Data: map[string]any{"line": "tail " + jobSecret[half:]}},
					},
				}
			},
			planted: []string{jobSecret, jobSecret[:half], jobSecret[half:]},
			keep:    []string{"infrastructure: harness unavailable", "(continued)"},
		},
		{
			name: "secret in a nested map in extra",
			event: func() *sentry.Event {
				return &sentry.Event{
					Message: "infrastructure: sandbox broken",
					Contexts: map[string]sentry.Context{
						"extra": {
							"job": map[string]any{
								"env": []any{"ANTHROPIC_API_KEY=" + antKey, "MESH_TOOL_KEY=" + envSecret},
								"deep": map[string]any{
									"deeper": map[string]any{"pat": patToken, "note": "openai " + openaiKey},
									"struct": struct{ Key string }{Key: "aws " + awsKey},
								},
							},
							"items": []string{"7", "token=" + consoleTok},
						},
					},
				}
			},
			planted: []string{antKey, envSecret, patToken, openaiKey, awsKey, consoleTok},
			keep:    []string{"infrastructure: sandbox broken", "ANTHROPIC_API_KEY=", "MESH_TOOL_KEY="},
		},
		{
			name: "url-encoded and base64 tokens in a query string",
			event: func() *sentry.Event {
				basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + ghsToken))
				return &sentry.Event{
					Message: "infrastructure: gateway read failed",
					Request: &sentry.Request{
						URL:         "https://api.runsheet.dev/mesh/v1?next=" + url.QueryEscape("/cb?code="+ghToken),
						QueryString: "state=ok&next=" + url.QueryEscape("/x?t="+ghToken) + "&auth=" + url.QueryEscape(basic) + "&access_token=" + url.QueryEscape(refreshTok),
						Headers: map[string]string{
							"Authorization": "Bearer " + jwt,
							"X-Forwarded":   "Basic " + basic,
							"Accept":        "application/json",
						},
						Cookies: "session=" + consoleTok + "; theme=dark",
						Data:    `{"grant_type":"refresh_token","refresh_token":"` + refreshTok + `"}`,
					},
				}
			},
			planted: []string{ghToken, ghsToken, jwt, refreshTok, consoleTok, base64.StdEncoding.EncodeToString([]byte("x-access-token:" + ghsToken)), url.QueryEscape(ghToken)},
			keep:    []string{"infrastructure: gateway read failed", "application/json", "state=ok", "theme="},
		},
		{
			name: "secret in an exception's stack frame vars",
			event: func() *sentry.Event {
				return &sentry.Event{
					Exception: []sentry.Exception{{
						Type:  "*node.InfrastructureError",
						Value: "infrastructure: session ended without success",
						Stacktrace: &sentry.Stacktrace{Frames: []sentry.Frame{{
							Function: "runBuild",
							AbsPath:  operatorHom + "/src/mesh-cli/internal/node/runner.go",
							Vars: map[string]any{
								"cred":   struct{ Token, Repo string }{Token: ghsToken, Repo: "runsheet/mesh-cli"},
								"header": "Authorization: Bearer " + jwt,
								"err":    errString("open " + operatorHom + "/.mesh/token: " + consoleTok),
								"secret": jobSecret,
							},
						}}},
					}},
				}
			},
			planted: []string{ghsToken, jwt, consoleTok, jobSecret, operatorHom},
			keep:    []string{"infrastructure: session ended without success", "runsheet/mesh-cli", "~/src/mesh-cli/internal/node/runner.go", "~/.mesh/token"},
		},
		{
			name: "secret as a map key",
			event: func() *sentry.Event {
				return &sentry.Event{
					Message: "infrastructure: harness unavailable",
					Tags:    map[string]string{openaiKey: "1", "item": "goal/3"},
					Contexts: map[string]sentry.Context{
						"extra": {consoleTok: "v", "nested": map[string]any{ghToken: map[string]any{antKey: true}}},
					},
					Breadcrumbs: []*sentry.Breadcrumb{{Data: map[string]any{jobSecret: 1}}},
					User:        sentry.User{Data: map[string]string{awsKey: "x"}},
				}
			},
			planted: []string{openaiKey, consoleTok, ghToken, antKey, jobSecret, awsKey},
			keep:    []string{"infrastructure: harness unavailable", "goal/3"},
		},
		{
			name: "token in a git remote URL",
			event: func() *sentry.Event {
				return &sentry.Event{
					Exception: []sentry.Exception{{
						Value: "credential_refused: push https://x-access-token:" + ghsToken + "@github.com/runsheet/mesh-cli.git: Write access to repository not granted",
					}},
					Breadcrumbs: []*sentry.Breadcrumb{
						{Message: "git remote set-url origin https://oauth2:" + patToken + "@github.com/runsheet/mesh-cli"},
						{Message: "fatal: unable to access 'https://" + ghToken + "@github.com/runsheet/x/': 403"},
					},
				}
			},
			planted: []string{ghsToken, patToken, ghToken},
			keep:    []string{"credential_refused: push https://x-access-token:", "@github.com/runsheet/mesh-cli.git", "Write access to repository not granted"},
		},
		{
			name: "the machine credential, the user and the host",
			event: func() *sentry.Event {
				return &sentry.Event{
					Message:    "infrastructure: refresh failed for " + tokenFile,
					ServerName: "dl360.lan.example.internal",
					User:       sentry.User{Email: "operator@example.com", IPAddress: "10.1.2.3", Username: "operator"},
					Contexts: map[string]sentry.Context{
						"node": {"aws": "aws_secret_access_key=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "home": operatorHom},
					},
				}
			},
			planted: []string{jwt, refreshTok, "operator@example.com", "10.1.2.3", "lan.example.internal", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", operatorHom},
			keep:    []string{"infrastructure: refresh failed for", `"server_name":"dl360"`, "https://api.runsheet.dev"},
		},
	}

	if len(cases) < 6 {
		t.Fatalf("only %d hostile cases", len(cases))
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := scrubbed(t, s, tc.event())
			for _, p := range tc.planted {
				if strings.Contains(got, p) {
					t.Errorf("planted secret %q survived:\n%s", p, got)
				}
			}
			for _, k := range tc.keep {
				if !strings.Contains(got, k) {
					t.Errorf("lost %q:\n%s", k, got)
				}
			}
		})
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestScrubLeavesPlainFailureText(t *testing.T) {
	msg := "infrastructure: harness unavailable; github token: refused; basic authentication flows; credential_refused: github credential: denied"
	ev := Scrub(&sentry.Event{Message: msg}, nil)
	if ev.Message != msg {
		t.Errorf("Scrub changed plain text:\n got %q\nwant %q", ev.Message, msg)
	}
}

func TestTokenFileSecrets(t *testing.T) {
	if got := TokenFileSecrets([]byte(consoleTok + "\n")); len(got) != 1 || got[0] != consoleTok {
		t.Errorf("console token: %q", got)
	}
	got := TokenFileSecrets([]byte(`{"accessToken":"a1","refreshToken":"r1","baseUrl":"https://x"}`))
	if len(got) != 2 {
		t.Errorf("machine credential: %q", got)
	}
}
