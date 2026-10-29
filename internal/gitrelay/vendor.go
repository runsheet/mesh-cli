package gitrelay

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// Each job may reach a different git vendor (Mesh v1 spec, "Many git
// vendors, and every write through the relay"): the brief names it, and
// a lease's grant picks its upstream from it. The relay is the same for
// every vendor at the git layer — smart HTTP and the push rules — and a
// Vendor is the small part that differs (wave 2.9): how a repository is
// named and laid out on the vendor's host, how the job's token is put on
// the request going upstream, and which of the vendor's response headers
// come back. One node serves jobs for any vendor at once.

// Vendor is one git vendor's adapter.
type Vendor interface {
	// Name is the vendor as a brief names it: github, gitlab, ….
	Name() string
	// Origin is the vendor's git origin when the brief names no
	// base_url: https://github.com for GitHub. A self-hosted vendor's
	// base_url names its own.
	Origin() string
	// RepoPath is the path repo is served at under the origin, or why
	// repo is not one of the vendor's: GitHub's owner/name is
	// owner/name.git, Azure DevOps' org/project/name is
	// org/project/_git/name.
	RepoPath(repo string) (string, error)
	// Authorize puts token on out, the request going upstream, in the
	// vendor's own shape.
	Authorize(out *http.Request, token string)
	// Returned are the only upstream response headers sent back to git.
	Returned() []string
}

// returned are the only upstream headers an adapter sends back: what
// git's smart HTTP reads, never a cookie, a challenge or a rate-limit
// header.
var returned = []string{"Content-Type", "Cache-Control", "Expires", "Pragma"}

// basicAuth is HTTP Basic user:password.
func basicAuth(out *http.Request, user, password string) {
	out.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+password)))
}

// repoSegments splits repo into its segments, refusing an empty, dot or
// dot-dot segment, and wanting between min and max of them (max 0 for
// no bound).
func repoSegments(vendor, repo string, min, max int, shape string) ([]string, error) {
	segs := strings.Split(repo, "/")
	bad := len(segs) < min || (max > 0 && len(segs) > max)
	for _, s := range segs {
		if s == "" || s == "." || s == ".." || strings.ContainsAny(s, " \t\r\n\\?#%") {
			bad = true
		}
	}
	if bad {
		return nil, fmt.Errorf("gitrelay: %q is not a %s repository (%s)", repo, vendor, shape)
	}
	return segs, nil
}

// GitHub is GitHub's adapter: owner/name, and HTTP Basic
// x-access-token:<token>, the shape an installation token takes.
type GitHub struct{}

// Name implements Vendor.
func (GitHub) Name() string { return "github" }

// Origin implements Vendor.
func (GitHub) Origin() string { return "https://github.com" }

// RepoPath implements Vendor.
func (GitHub) RepoPath(repo string) (string, error) {
	if _, err := repoSegments("github", repo, 2, 2, "owner/name"); err != nil {
		return "", err
	}
	return repo + ".git", nil
}

// Authorize implements Vendor.
func (GitHub) Authorize(out *http.Request, token string) { basicAuth(out, "x-access-token", token) }

// Returned implements Vendor.
func (GitHub) Returned() []string { return returned }

// GitLab is GitLab's adapter, gitlab.com's or a self-hosted one's:
// group/project, any depth of subgroups between, and HTTP Basic
// oauth2:<token>, the shape a project access token or an OAuth token
// takes.
type GitLab struct{}

// Name implements Vendor.
func (GitLab) Name() string { return "gitlab" }

// Origin implements Vendor.
func (GitLab) Origin() string { return "https://gitlab.com" }

// RepoPath implements Vendor.
func (GitLab) RepoPath(repo string) (string, error) {
	if _, err := repoSegments("gitlab", repo, 2, 0, "group/project, with any subgroups between"); err != nil {
		return "", err
	}
	return repo + ".git", nil
}

// Authorize implements Vendor.
func (GitLab) Authorize(out *http.Request, token string) { basicAuth(out, "oauth2", token) }

// Returned implements Vendor.
func (GitLab) Returned() []string { return returned }

// Bitbucket is Bitbucket Cloud's adapter: workspace/repo, and HTTP
// Basic x-token-auth:<token>, the shape a repository access token or an
// OAuth token takes.
type Bitbucket struct{}

// Name implements Vendor.
func (Bitbucket) Name() string { return "bitbucket" }

// Origin implements Vendor.
func (Bitbucket) Origin() string { return "https://bitbucket.org" }

// RepoPath implements Vendor.
func (Bitbucket) RepoPath(repo string) (string, error) {
	if _, err := repoSegments("bitbucket", repo, 2, 2, "workspace/repo"); err != nil {
		return "", err
	}
	return repo + ".git", nil
}

// Authorize implements Vendor.
func (Bitbucket) Authorize(out *http.Request, token string) { basicAuth(out, "x-token-auth", token) }

// Returned implements Vendor.
func (Bitbucket) Returned() []string { return returned }

// Gitea is Gitea's (and Forgejo's) adapter: owner/name on the tenant's
// own host, and `Authorization: token <token>`. Designed in for v1; its
// Latchkey mint comes later.
type Gitea struct{}

// Name implements Vendor.
func (Gitea) Name() string { return "gitea" }

// Origin implements Vendor: Gitea is self-hosted, and gitea.com is its
// public instance.
func (Gitea) Origin() string { return "https://gitea.com" }

// RepoPath implements Vendor.
func (Gitea) RepoPath(repo string) (string, error) {
	if _, err := repoSegments("gitea", repo, 2, 2, "owner/name"); err != nil {
		return "", err
	}
	return repo + ".git", nil
}

// Authorize implements Vendor.
func (Gitea) Authorize(out *http.Request, token string) {
	out.Header.Set("Authorization", "token "+token)
}

// Returned implements Vendor.
func (Gitea) Returned() []string { return returned }

// AzureDevOps is Azure DevOps' adapter: org/project/repo, served at
// org/project/_git/repo, and HTTP Basic with an empty user and the
// token as the password, the shape a PAT takes. Designed in for v1; its
// Latchkey mint comes later.
type AzureDevOps struct{}

// Name implements Vendor.
func (AzureDevOps) Name() string { return "azure_devops" }

// Origin implements Vendor.
func (AzureDevOps) Origin() string { return "https://dev.azure.com" }

// RepoPath implements Vendor.
func (AzureDevOps) RepoPath(repo string) (string, error) {
	segs, err := repoSegments("azure_devops", repo, 3, 3, "org/project/repo")
	if err != nil {
		return "", err
	}
	return segs[0] + "/" + segs[1] + "/_git/" + segs[2], nil
}

// Authorize implements Vendor.
func (AzureDevOps) Authorize(out *http.Request, token string) { basicAuth(out, "", token) }

// Returned implements Vendor.
func (AzureDevOps) Returned() []string { return returned }

// vendors are the adapters a brief may name, by name.
var vendors = map[string]Vendor{
	"github":       GitHub{},
	"gitlab":       GitLab{},
	"bitbucket":    Bitbucket{},
	"gitea":        Gitea{},
	"azure_devops": AzureDevOps{},
}

// DefaultVendor is the vendor of a policy that names none: GitHub, as
// every brief before vendors were named meant.
const DefaultVendor = "github"

// Vendors are the names of every vendor the relay has an adapter for.
func Vendors() []string {
	var out []string
	for n := range vendors {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// VendorNamed answers the adapter for name ("" is DefaultVendor), and
// an error naming the vendors there are when the relay has none for it.
func VendorNamed(name string) (Vendor, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		name = DefaultVendor
	}
	if v, ok := vendors[name]; ok {
		return v, nil
	}
	return nil, fmt.Errorf("gitrelay: no adapter for git vendor %q (this node has %s)", name, strings.Join(Vendors(), ", "))
}
