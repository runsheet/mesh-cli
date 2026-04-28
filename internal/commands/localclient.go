package commands

import (
	"context"
	"io"
	"net/http"
	"net/url"

	iclient "projectmesh.io/mesh-cli/internal/client"
	"projectmesh.io/mesh-cli/internal/localsock"
	itpl "projectmesh.io/mesh-cli/internal/template"
)

// localClient is the HTTP-over-Unix client for the mesh daemon.
type localClient struct {
	c *iclient.Client
}

// newLocalClient returns a client pointed at ~/.mesh/daemon.sock.
func newLocalClient() (*localClient, error) {
	c, err := iclient.New()
	if err != nil {
		return nil, err
	}
	return &localClient{c: c}, nil
}

func (c *localClient) do(ctx context.Context, method, path string, body any, out any) error {
	return c.c.Do(ctx, method, path, body, out)
}

func (c *localClient) listSessions(ctx context.Context, all bool) ([]localsock.SessionView, error) {
	path := "/sessions"
	if all {
		path = "/sessions?all=true"
	}
	var out []localsock.SessionView
	if err := c.c.Do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *localClient) createSession(ctx context.Context, req localsock.CreateSessionRequest) (*localsock.CreateSessionResponse, error) {
	var out localsock.CreateSessionResponse
	if err := c.c.Do(ctx, http.MethodPost, "/sessions", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *localClient) inspectSession(ctx context.Context, name string) (map[string]any, error) {
	var out map[string]any
	if err := c.c.Do(ctx, http.MethodGet, "/sessions/"+url.PathEscape(name), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *localClient) removeSession(ctx context.Context, name string) error {
	return c.c.Do(ctx, http.MethodDelete, "/sessions/"+url.PathEscape(name), nil, nil)
}

func (c *localClient) restartSession(ctx context.Context, name string) (map[string]string, error) {
	var out map[string]string
	if err := c.c.Do(ctx, http.MethodPost, "/sessions/"+url.PathEscape(name)+"/restart", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *localClient) pushAgent(ctx context.Context, name string, req localsock.PushSessionRequest) (int, []byte, error) {
	return c.c.DoRaw(ctx, http.MethodPost, "/sessions/"+url.PathEscape(name)+"/push", req)
}

func (c *localClient) pullAgent(ctx context.Context, req localsock.PullSessionRequest) (int, []byte, error) {
	return c.c.DoRaw(ctx, http.MethodPost, "/cluster/pull", req)
}

func (c *localClient) updateSession(ctx context.Context, name string) (map[string]any, error) {
	var out map[string]any
	if err := c.c.Do(ctx, http.MethodPost, "/sessions/"+url.PathEscape(name)+"/update", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *localClient) listTemplates(ctx context.Context) ([]itpl.TemplateCacheEntry, error) {
	var out []itpl.TemplateCacheEntry
	if err := c.c.Do(ctx, http.MethodGet, "/templates", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *localClient) pullTemplate(ctx context.Context, source string) (*localsock.PullTemplateResponse, error) {
	var out localsock.PullTemplateResponse
	if err := c.c.Do(ctx, http.MethodPost, "/templates/pull", localsock.PullTemplateRequest{Source: source}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *localClient) logs(ctx context.Context, name string, follow bool, out io.Writer) error {
	return c.c.Logs(ctx, name, follow, out)
}

func (c *localClient) attach(ctx context.Context, name string, stdin io.Reader, stdout io.Writer, onDetach func() bool) error {
	return c.c.Attach(ctx, name, stdin, stdout, onDetach)
}
