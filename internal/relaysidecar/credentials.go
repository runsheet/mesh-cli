// Package relaysidecar is a sandboxed lease's relay as a sidecar in the
// lease's own pod (projectmesh-io/api ADR 001 amendment "relay
// sidecar"). The node daemon holds the node's machine credential, the
// model account key and every live lease's session; the relay parses
// what a pod sends it (HTTP, the git protocol, model calls). Run in the
// daemon, a bug in the relay would expose all of it. Run as `mesh
// relay-sidecar` in a container of the lease's pod, under a UID of its
// own, it holds one lease's short-lived credentials and nothing else.
//
// The pieces:
//
//   - Credentials: what the node hands the sidecar, once, in a file on a
//     tmpfs only the sidecar has (Install, Load). Only lease-scoped
//     credentials: the lease's DPoP key and session token, the job's git
//     token, and the brief's push rules and access policy. The model
//     account key, the machine credential, the request credential that
//     mints session tokens and evidence signing stay on the node.
//   - Sidecar: the routes the session uses, on the pod's loopback: /git/,
//     /mcp/<server>, /latchkey/<vendor>/, /model/<provider>/ and a
//     CONNECT egress proxy. Every upstream connection it opens goes
//     through the node's tunnel (Dialer).
//   - Node: what the node serves on the lease's one socket: the tunnel
//     (CONNECT to an allowed host, TLS end to end, no HTTP read past the
//     CONNECT line), the model forwarder, the node tools, renewal and
//     the sidecar's AccessRecords.
package relaysidecar

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"projectmesh.io/mesh-cli/internal/dpop"
	"projectmesh.io/mesh-cli/internal/gitrelay"
	"projectmesh.io/mesh-cli/internal/meshapi"
)

// CredentialsFile is the credential file's name in the sidecar's tmpfs.
const CredentialsFile = "credentials"

// credentialsMax bounds the file: a key, two tokens and a policy.
const credentialsMax = 1 << 20

// Credentials are one lease's, as the sidecar holds them. Nothing in
// them outlives the lease or spans leases.
type Credentials struct {
	// Lease is the lease they are for.
	Lease string `json:"lease"`
	// Key is the lease's DPoP key, which its session tokens are bound
	// to.
	Key dpop.Handoff `json:"key"`
	// Session is the lease's mesh-session token of the moment.
	Session Token `json:"session"`
	// Latchkey is Latchkey's origin, where /latchkey/<vendor>/ goes; ""
	// when the brief allows no vendor API.
	Latchkey string `json:"latchkey,omitempty"`
	// API is the brief's access.api: the vendor calls the session may
	// make through Latchkey's proxy.
	API []meshapi.APIAccess `json:"api,omitempty"`
	// Servers are the product's MCP servers, reached on the session
	// token with a DPoP proof.
	Servers []Server `json:"servers,omitempty"`
	// Git is the job's git, nil when it has none.
	Git *Git `json:"git,omitempty"`
}

// Server is a product MCP server: its name at /mcp/<name> and its URL.
type Server struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// Git is the job's git as the sidecar's relay serves it.
type Git struct {
	// Policy is the brief's: the repository, read or write, the push
	// rules, the vendor and its base.
	Policy gitrelay.Policy `json:"policy"`
	// Upstream is the vendor's git origin, "" for the vendor's own.
	Upstream string `json:"upstream,omitempty"`
	// Token is the job's git token of the moment.
	Token Token `json:"token"`
	// Checkout is the lease's checkout as the sidecar has it mounted
	// (read-only), where a push's fast-forward is proved.
	Checkout string `json:"checkout,omitempty"`
}

// Token is a credential and when it ends.
type Token struct {
	Value     string    `json:"value"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
}

// String never prints the value.
func (t Token) String() string {
	if t.Value == "" {
		return "Token(none)"
	}
	return "Token(***, expires " + t.ExpiresAt.UTC().Format(time.RFC3339) + ")"
}

// GoString is String.
func (t Token) GoString() string { return t.String() }

// Format prints String for every verb.
func (t Token) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(t.String())) }

// String names the lease alone.
func (c Credentials) String() string { return "relaysidecar.Credentials(lease " + c.Lease + ")" }

// GoString is String.
func (c Credentials) GoString() string { return c.String() }

// Format prints String for every verb.
func (c Credentials) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(c.String())) }

// Validate refuses credentials a sidecar cannot serve on.
func (c Credentials) Validate() error {
	if strings.TrimSpace(c.Lease) == "" {
		return errors.New("relaysidecar: the credentials name no lease")
	}
	if _, err := dpop.KeyFromHandoff(c.Key); err != nil {
		return fmt.Errorf("relaysidecar: the credentials' key: %w", err)
	}
	for _, s := range c.Servers {
		if s.Name == "" || s.URL == "" {
			return fmt.Errorf("relaysidecar: a server without a name or url: %q", s.Name)
		}
	}
	if len(c.API) > 0 && c.Latchkey == "" {
		return errors.New("relaysidecar: vendor API rules without a Latchkey origin")
	}
	if c.Git != nil && c.Git.Policy.Repo == "" {
		return errors.New("relaysidecar: git without a repository")
	}
	return nil
}

// Install reads credentials from r (the sidecar's stdin, from the
// node's `podman exec -i`) and writes them into dir — the sidecar's own
// tmpfs — as CredentialsFile, mode 0400. dir must be a directory of this
// user's alone (mode 0700 or tighter); a file already there is replaced
// whole (renewal by the node writes it again), never written in place.
// The value goes from stdin to the file and nowhere else: not argv, not
// the environment, not a log.
func Install(r io.Reader, dir string) error {
	raw, err := io.ReadAll(io.LimitReader(r, credentialsMax+1))
	if err != nil {
		return fmt.Errorf("relaysidecar: reading the credentials: %w", err)
	}
	if len(raw) > credentialsMax {
		return errors.New("relaysidecar: the credentials are too large")
	}
	var c Credentials
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return fmt.Errorf("relaysidecar: the credentials are not JSON of their shape: %w", err)
	}
	if err := c.Validate(); err != nil {
		return err
	}
	if err := privateDir(dir); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".credentials-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o400); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, filepath.Join(dir, CredentialsFile))
}

// Load reads the credentials at path. The file must be this user's own
// and readable by no one else (0400 or 0600), in a directory of this
// user's alone; anything looser is refused, since it means someone
// besides the sidecar may have read or written it.
func Load(path string) (Credentials, error) {
	if err := privateDir(filepath.Dir(path)); err != nil {
		return Credentials{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return Credentials{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return Credentials{}, err
	}
	if !fi.Mode().IsRegular() {
		return Credentials{}, fmt.Errorf("relaysidecar: %s is not a regular file", path)
	}
	if fi.Mode().Perm()&0o177 != 0 {
		return Credentials{}, fmt.Errorf("relaysidecar: %s is mode %#o; it must be readable by its owner alone", path, fi.Mode().Perm())
	}
	if err := ownedBySelf(fi); err != nil {
		return Credentials{}, fmt.Errorf("relaysidecar: %s: %w", path, err)
	}
	raw, err := io.ReadAll(io.LimitReader(f, credentialsMax+1))
	if err != nil {
		return Credentials{}, err
	}
	var c Credentials
	if err := json.Unmarshal(raw, &c); err != nil {
		return Credentials{}, fmt.Errorf("relaysidecar: %s is not credentials: %w", path, err)
	}
	return c, c.Validate()
}

// privateDir holds dir to a real directory of this user's that no one
// else can enter.
func privateDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("relaysidecar: %s is not a directory", dir)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("relaysidecar: %s is mode %#o; it must be its owner's alone", dir, fi.Mode().Perm())
	}
	if err := ownedBySelf(fi); err != nil {
		return fmt.Errorf("relaysidecar: %s: %w", dir, err)
	}
	return nil
}
