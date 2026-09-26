package gatewaytest

import (
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// GitHub serves bare repos under root over smart HTTP, as GitHub would:
// a request needs basic auth, and a password reaches only the repos
// access grants it ("*" every one) — an installation token its one
// repository, the operator's own login all of them. It skips the test
// where git has no http backend.
func GitHub(t testing.TB, root string, access map[string]string) *httptest.Server {
	t.Helper()
	execPath, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Skip("no git exec path")
	}
	backend := filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skip("no git-http-backend")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok {
			w.Header().Set("WWW-Authenticate", `Basic realm="github"`)
			http.Error(w, "auth", http.StatusUnauthorized)
			return
		}
		repo, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), ".git/")
		if grant := access[pass]; grant != "*" && grant != repo {
			http.Error(w, "Write access to repository not granted.", http.StatusForbidden)
			return
		}
		h := &cgi.Handler{Path: backend, Root: "/", InheritEnv: []string{"PATH"},
			Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER=" + user}}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}
