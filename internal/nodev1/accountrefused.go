package nodev1

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/sessionproxy"
)

// A model account its vendor refuses is the account's fault, not the
// job's (ADR-048; projectmesh-io/api ADR 001 amendment, account
// failover). On the vendor's final answer to a model call of a lease
// that runs on the job's account — after the relay's one re-read of the
// credential on a 401 —
//
//   - 401, any error type: the credential is not accepted;
//   - 403, any error type the vendor sent: the account may not do this;
//
// the node ends the session at once and hands the lease back
// account_refused, naming the account and the vendor's error type and
// message, bounded and redacted of the lease's values. Mesh cools the
// account down and offers the job again on another; the hand-back is
// not counted against the job. 429 (the account's limits), 5xx and 529
// (the vendor's outage), any other 4xx (the request's own) and a 403
// the relay answered itself (status 0 in the call: nothing reached the
// vendor) are not refusals. A session on the node's own login is not
// Mesh's account, and never hands back account_refused.

// accountRefusedError is a lease's account refused by its vendor.
type accountRefusedError struct {
	ref, vendor     string
	status          int
	errType, errMsg string
	path            string
}

func (e *accountRefusedError) Error() string {
	return fmt.Sprintf("the model account %s was refused: %s %s answered %d: %s", e.ref, e.vendor, e.path, e.status, vendorErrorLine(e.errType, e.errMsg))
}

// refusedStatus is whether a model call's final status is the vendor
// refusing the account's credential.
func refusedStatus(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}

// vendorErrorLine is a vendor error as a log line says it.
func vendorErrorLine(typ, msg string) string {
	switch {
	case typ == "" && msg == "":
		return "no error the vendor named"
	case typ == "":
		return msg
	case msg == "":
		return typ
	}
	return typ + ": " + msg
}

// noteRefusal keeps the first refusal of the job's account and ends the
// lease on it (onRefused).
func (a *sessionAccount) noteRefusal(c sessionproxy.ModelCall, logf func(string, ...any), lease string) {
	if a == nil || !refusedStatus(c.Status) {
		return
	}
	a.mu.Lock()
	if a.cred == nil || a.cred.AccountRef == "" || a.refusal != nil {
		a.mu.Unlock()
		return
	}
	e := &accountRefusedError{ref: a.cred.AccountRef, vendor: c.Provider, status: c.Status, errType: c.ErrorType, errMsg: c.ErrorMessage,
		path: c.Method + " " + c.Path}
	a.refusal = e
	end := a.onRefused
	a.mu.Unlock()
	logf("nodev1: lease %s: %v; the lease is handed back account_refused", lease, e)
	if end != nil {
		end(e)
	}
}

// refused is the job's account's first refusal by its vendor, nil when
// none.
func (a *sessionAccount) refused() *accountRefusedError {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.refusal
}

// accountRefused is the lease's account refusal that ends it: the cause
// the node ended the lease with, or — the session gone first, with no
// finish — the refusal its relay saw. A lease that finished keeps its
// finish.
func (o outcome) accountRefused() *accountRefusedError {
	var e *accountRefusedError
	if errors.As(o.cause, &e) {
		return e
	}
	if e = o.account.refused(); e != nil && o.finish == nil {
		return e
	}
	return nil
}

// handBackRefused hands the lease back account_refused with the
// vendor's answer. A Mesh that does not know the cause (422) hears
// cannot_run with the same reason, as before ADR-048.
func (r *Runtime) handBackRefused(id string, e *accountRefusedError, reason, sessionRef string) string {
	ctx, cancel := context.WithTimeout(context.Background(), reportTimeout)
	defer cancel()
	req := meshapi.HandBackRequest{Cause: meshapi.CauseAccountRefused, Reason: clip(r.redact(reason), maxReason), SessionRef: sessionRef,
		Account: &meshapi.RefusedAccount{AccountRef: e.ref, Vendor: e.vendor, Status: e.status,
			ErrorType: r.redact(e.errType), ErrorMessage: r.redact(e.errMsg)}}
	st, err := r.API.HandBack(ctx, id, req)
	var me *meshapi.Error
	if errors.As(err, &me) && me.Status == http.StatusUnprocessableEntity {
		r.logf("nodev1: lease %s: Mesh does not take account_refused (%v); handing back cannot_run", id, err)
		r.handBack(id, meshapi.CauseCannotRun, reason, sessionRef)
		return meshapi.CauseCannotRun
	}
	switch {
	case err != nil && meshapi.LeaseGone(err):
		r.logf("nodev1: lease %s was no longer the node's to hand back (%s)", id, meshapi.CauseAccountRefused)
	case err != nil:
		r.logf("nodev1: lease %s: hand-back (%s): %v", id, meshapi.CauseAccountRefused, err)
	default:
		r.logf("nodev1: lease %s handed back (%s, account %s); job %s is %s, %d hand-back(s) counted", id, meshapi.CauseAccountRefused, e.ref, st.JobID, st.Status, st.Handbacks)
	}
	return meshapi.CauseAccountRefused
}
