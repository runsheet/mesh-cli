package gitrelay

import (
	"fmt"
	"strings"
)

// A push takes a lease (mesh-specs gaps, 35; projectmesh-io/api ADR 001
// amendment "push lease"). A rebased branch is never a fast-forward of
// where it was, so a fast-forward-only rule would refuse every rebase
// job's push. A leased push replaces a ref only from the commit the job
// found it at — its head — and never overwrites a push someone else made
// since: git's --force-with-lease=<ref>:<head>, which sends the head as
// the update's old commit, and the vendor applies the update only if the
// ref is still there.
//
// The lease is the job's to ask for, never a default of the relay: a
// push without one stays fast-forward only, as before. It is asked for
// with the LeaseHeader on the push's requests, which the push tool (and a
// session's own git, with http.extraHeader) sends and the relay never
// passes upstream. Only a ref the policy names a head for takes one — the
// brief's push_to — and only from that head: what the job found it at, or
// what the job itself pushed there since. A lease on another ref, from
// another commit, or on a ref that moved since is refused before the
// vendor sees anything.

// LeaseHeader asks for a leased push: "<ref> <sha>", one per ref, where
// sha is the commit the ref must be at for the push to replace it (the
// zero id for a ref that must not exist yet).
const LeaseHeader = "Mesh-Push-Lease"

// ZeroID is the id git names a ref that does not exist by.
const ZeroID = zeroID

// readLeases reads a push's LeaseHeader values: the commit each leased
// ref must be at. A value that is not "<full ref> <commit id>" is an
// error.
func readLeases(values []string) (map[string]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := map[string]string{}
	for _, v := range values {
		for _, one := range strings.Split(v, ",") {
			f := strings.Fields(one)
			if len(f) == 0 {
				continue
			}
			if len(f) != 2 || !strings.HasPrefix(f[0], "refs/") || !objectID(f[1]) {
				return nil, fmt.Errorf("%q is not a lease: it is %s: <ref> <commit>", strings.TrimSpace(one), LeaseHeader)
			}
			out[f[0]] = f[1]
		}
	}
	return out, nil
}

// Head is the commit the relay holds for ref, which a leased push of it
// must replace: where the job found it (Policy.Heads), or what this
// relay last pushed there for the job. ok is false for a ref that takes
// no lease.
func (r *Relay) Head(ref string) (sha string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.initHeads()
	sha, ok = r.heads[ref]
	return sha, ok
}

// SetHead names where the job found ref: the commit a leased push of it
// must replace, ZeroID when the ref did not exist.
func (r *Relay) SetHead(ref, sha string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.initHeads()
	r.heads[ref] = sha
}

// moved keeps what an ok push put at each ref that takes a lease: the
// job's own push is where a later lease of it replaces from.
func (r *Relay) moved(cmds []command) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.initHeads()
	for _, c := range cmds {
		if _, ok := r.heads[c.ref]; ok {
			r.heads[c.ref] = c.new
		}
	}
}

// initHeads copies the policy's heads; r.mu is held.
func (r *Relay) initHeads() {
	if r.heads != nil {
		return
	}
	r.heads = map[string]string{}
	for ref, sha := range r.Policy.Heads {
		if sha == "" {
			sha = zeroID
		}
		r.heads[ref] = sha
	}
}

// checkLease refuses a leased update the relay cannot allow: c's ref
// takes no lease, the lease names a commit other than the ref's head,
// or the ref is not at the head now (someone pushed since). It answers
// "" for an update that replaces the ref from its head, which the vendor
// then applies only if the ref is still there.
func (r *Relay) checkLease(c command, lease string) string {
	head, ok := r.Head(c.ref)
	if !ok {
		return c.ref + " takes no lease: a lease replaces only the job's push_to, from where the job found it"
	}
	if lease != head {
		return fmt.Sprintf("the lease on %s names %s, but the job found it at %s: a lease replaces it only from there", c.ref, leaseShort(lease), leaseShort(head))
	}
	if c.old != head {
		return fmt.Sprintf("%s is at %s now, not %s where the job found it: someone pushed since, and a leased push never overwrites that", c.ref, leaseShort(c.old), leaseShort(head))
	}
	return ""
}

// leaseShort names a commit in a refusal: its full id, or "nothing" for
// a ref that does not exist.
func leaseShort(sha string) string {
	if strings.Trim(sha, "0") == "" {
		return "nothing (no such ref)"
	}
	return sha
}

// LeasedPushArgs are the git arguments of a push of sha to ref through
// target under lease, the commit ref must be at (ZeroID or "" for a ref
// that must not exist): the LeaseHeader for the relay, and git's own
// --force-with-lease, so git refuses a stale lease before it sends
// anything.
func LeasedPushArgs(target, sha, ref, lease string) []string {
	expect := lease
	if strings.Trim(lease, "0") == "" {
		lease, expect = zeroID, ""
	}
	return []string{"-c", "http.extraHeader=" + LeaseHeader + ": " + ref + " " + lease,
		"push", "--porcelain", "--no-verify", "--force-with-lease=" + ref + ":" + expect, target, sha + ":" + ref}
}

// StaleLease says whether git's push output is its own refusal of a
// lease: the ref is not where the lease says, so nothing was sent.
func StaleLease(out string) bool {
	return strings.Contains(out, "stale info")
}

// LeaseHeadAsked is the lease a push tool's caller asks for without
// naming a commit: the ref's head, where the job found it.
const LeaseHeadAsked = "head"

// StaleLeaseReason is why a lease git refused itself was refused: ref is
// at now (ZeroID or "" for gone), not lease.
func StaleLeaseReason(ref, lease, now string) string {
	if now == "" {
		now = zeroID
	}
	return fmt.Sprintf("%s is at %s now, not %s where the job found it: someone pushed since, and a leased push never overwrites that", ref, leaseShort(now), leaseShort(lease))
}

// RemoteHead reads what ref names in git ls-remote's output, ZeroID when
// it names nothing.
func RemoteHead(lsRemote, ref string) string {
	for _, line := range strings.Split(lsRemote, "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[1] == ref {
			return f[0]
		}
	}
	return zeroID
}
