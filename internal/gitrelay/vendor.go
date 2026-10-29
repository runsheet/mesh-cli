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
// Vendor is the small part that differs: how the job's token is put on
// the request going upstream, and which of the vendor's response
// headers come back. GitHub is the first; GitLab, Bitbucket, Gitea and
// Azure DevOps are each one more adapter in vendors (wave 2.9).

// Vendor is one git vendor's adapter.
type Vendor interface {
	// Name is the vendor as a brief names it: github, gitlab, ….
	Name() string
	// Origin is the vendor's git origin when the brief names no
	// base_url: https://github.com for GitHub.
	Origin() string
	// Authorize puts token on out, the request going upstream, in the
	// vendor's own shape.
	Authorize(out *http.Request, token string)
	// Returned are the only upstream response headers sent back to git.
	Returned() []string
}

// GitHub is GitHub's adapter: HTTP Basic x-access-token:<token>, the
// shape an installation token takes.
type GitHub struct{}

// Name implements Vendor.
func (GitHub) Name() string { return "github" }

// Origin implements Vendor.
func (GitHub) Origin() string { return "https://github.com" }

// Authorize implements Vendor.
func (GitHub) Authorize(out *http.Request, token string) {
	out.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token)))
}

// Returned implements Vendor.
func (GitHub) Returned() []string { return returned }

// returned are the only upstream headers GitHub's adapter sends back:
// what git's smart HTTP reads, never a cookie or a rate-limit header.
var returned = []string{"Content-Type", "Cache-Control", "Expires", "Pragma"}

// vendors are the adapters a brief may name, by name.
var vendors = map[string]Vendor{
	"github": GitHub{},
}

// DefaultVendor is the vendor of a policy that names none: GitHub, as
// every brief before vendors were named meant.
const DefaultVendor = "github"

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
	var known []string
	for n := range vendors {
		known = append(known, n)
	}
	sort.Strings(known)
	return nil, fmt.Errorf("gitrelay: no adapter for git vendor %q (this node has %s)", name, strings.Join(known, ", "))
}
