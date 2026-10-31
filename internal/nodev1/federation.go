package nodev1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"projectmesh.io/mesh-cli/internal/egress"
	"projectmesh.io/mesh-cli/internal/meshapi"
)

// The relay mints a job's federation token and trades it at the cloud
// (mesh-specs gap 32; ADR-046's amendment). Each FederationTarget ref in
// the brief's access.federation is minted on the lease's session token —
//
//	POST <Latchkey>/v1/federation/token {target, purpose}
//	Authorization: DPoP <mesh-session token>, DPoP: <proof>
//	→ {subject_token, issued_token_type, expires_in, target_ref, cloud, exchange}
//
// (latchkeyid/latchkey #693; Latchkey reads the job from Mesh, requires
// its lease live and its access.federation to name the target, and
// signs a five-minute subject token naming the job, kind and lease) —
// and the node trades the subject token at the cloud itself, in its own
// process: for gcp, STS's token exchange at exchange.sts_url, then
// generateAccessToken for exchange.service_account at
// exchange.iam_credentials_url, each URL checked to be Google's before a
// token is sent there. The cloud token is injected as bearer on the
// kind's egress hosts under *.googleapis.com (an egress.InjectRule, as
// gap 28's), replacing whatever Authorization the sandbox sent; the pod
// holds the placeholder `mesh` in GOOGLE_OAUTH_ACCESS_TOKEN and
// CLOUDSDK_AUTH_ACCESS_TOKEN so the Google provider and gcloud send one.
//
// The subject token, STS's federated token and the cloud token are held
// in this process's memory alone, each registered with the lease's
// redactor as it is had, before it is used; minted once as the lease
// opens (a refusal hands the lease back: product for a refusal of the
// product's configuration, credential_timeout for Latchkey or the cloud
// unreachable, gap 14), again on the first request within a minute of
// the cloud token's end, and dropped at the lease's end. Nothing is kept
// across leases. AWS (AssumeRoleWithWebIdentity, then aws_sigv4) and
// Azure (a client assertion, then bearer) are not traded yet: a target of
// either hands the lease back, said; tradeFor is the seam.

// federationPath is Latchkey's federation-token route.
const federationPath = "/v1/federation/token"

// federationRefreshBefore is how long before the cloud token's end it is
// minted and traded again (gap 32 item 4).
const federationRefreshBefore = time.Minute

// federationKind names a federation target in the audit and the
// redactor's marks.
const federationKind = "federation"

// The federation codes the relay names beside Latchkey's, in
// credential_refused's message.
const (
	federationGCPSTS        = "gcp_sts"
	federationGCPIAM        = "gcp_iam"
	federationTargetMissing = "target_not_found"
	federationCloudNotYet   = "cloud_not_supported"
)

// Google's endpoints a gcp target's subject token is traded at: only
// these hosts, on https, are ever sent a token.
const (
	googleSTSHost = "sts.googleapis.com"
	googleIAMHost = "iamcredentials.googleapis.com"
	// googleAPIs are the hosts a gcp target's cloud token is written on
	googleAPIs = "*.googleapis.com"
	// googleScope is what STS is asked for when the target names no
	// scopes, and always when the token is then traded for a service
	// account's: generateAccessToken takes no narrower one
	googleScope = "https://www.googleapis.com/auth/cloud-platform"
	// tokenExchange and accessTokenType are RFC 8693's
	tokenExchange   = "urn:ietf:params:oauth:grant-type:token-exchange"
	accessTokenType = "urn:ietf:params:oauth:token-type:access_token"
	jwtTokenType    = "urn:ietf:params:oauth:token-type:jwt"
	// googleDefaultLifetime is the cloud token's life when the exchange
	// names none
	googleDefaultLifetime = time.Hour
)

// googleTokenEnv are the variables the Google provider and gcloud read an
// access token from: the pod holds the placeholder in each.
var googleTokenEnv = []string{"CLOUDSDK_AUTH_ACCESS_TOKEN", "GOOGLE_OAUTH_ACCESS_TOKEN"}

// federationToken is Latchkey's answer: never logged.
type federationToken struct {
	SubjectToken    string          `json:"subject_token"`
	IssuedTokenType string          `json:"issued_token_type"`
	ExpiresIn       int64           `json:"expires_in"`
	TargetRef       string          `json:"target_ref"`
	Cloud           string          `json:"cloud"`
	Exchange        json.RawMessage `json:"exchange"`
}

// FederationToken mints ref's subject token on the session token, for
// purpose. A refusal is a *latchkeyError carrying Latchkey's status and
// code; the route answers a bare 404 for an org without the federation
// feature, read as target_not_found.
func (s *sessionTokens) FederationToken(ctx context.Context, ref, purpose string) (*federationToken, error) {
	var out federationToken
	err := s.onToken(ctx, http.MethodPost, federationPath, map[string]string{"target": ref, "purpose": purpose}, &out)
	var le *latchkeyError
	if errors.As(err, &le) && le.Status == http.StatusNotFound && le.Code == "" {
		le.Code = federationTargetMissing
	}
	if err != nil {
		return nil, err
	}
	if out.SubjectToken == "" {
		return nil, errors.New("Latchkey answered federation target " + ref + " with no subject token")
	}
	return &out, nil
}

// gcpExchange is a gcp target's exchange, as Latchkey answers it.
type gcpExchange struct {
	STSURL                   string   `json:"sts_url"`
	Audience                 string   `json:"audience"`
	WorkloadIdentityProvider string   `json:"workload_identity_provider"`
	ServiceAccount           string   `json:"service_account"`
	Scopes                   []string `json:"scopes"`
	IAMCredentialsURL        string   `json:"iam_credentials_url"`
	SubjectTokenType         string   `json:"subject_token_type"`
	// Duration is the cloud token's life in seconds
	Duration int64 `json:"duration"`
}

// cloudError is the cloud refusing a trade, or not reached: Step is
// gcp_sts or gcp_iam, Code and Message Google's. Never a token. Status 0
// is the node refusing to send a token to an exchange URL not Google's.
type cloudError struct {
	Step        string
	Status      int
	Code        string
	Message     string
	Unreachable bool
}

func (e *cloudError) Error() string {
	if e.Unreachable {
		return fmt.Sprintf("%s unreachable: %s", e.Step, e.Message)
	}
	msg := strings.TrimSpace(strings.Trim(e.Code+": "+e.Message, ": "))
	if e.Status == 0 {
		return fmt.Sprintf("%s refused: %s", e.Step, msg)
	}
	return fmt.Sprintf("%s refused: %s (%d)", e.Step, msg, e.Status)
}

// productsConfig says the cloud refused what the target is configured
// as (its provider, its audience, the service account's binding), not
// that it could not be reached: a 4xx other than a rate limit.
func (e *cloudError) productsConfig() bool {
	return !e.Unreachable && e.Status >= 400 && e.Status < 500 && e.Status != http.StatusTooManyRequests
}

// unsupportedCloud is a target of a cloud the node does not trade yet.
type unsupportedCloud struct{ cloud string }

func (e *unsupportedCloud) Error() string {
	return fmt.Sprintf("%s: this node trades only gcp federation targets, not %q", federationCloudNotYet, e.cloud)
}

// googleURL is raw when it is https on host exactly, with no user, no
// port but 443 and no fragment: Google's endpoint, and nowhere else a
// token goes.
func googleURL(raw, host string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("the exchange's url %q is not a url", raw)
	}
	if u.Scheme != "https" || u.User != nil || u.Fragment != "" || !strings.EqualFold(u.Hostname(), host) || (u.Port() != "" && u.Port() != "443") {
		return nil, fmt.Errorf("the exchange's url %q is not Google's %s on https; no token is sent there", raw, host)
	}
	return u, nil
}

// gcpTrader trades a gcp target's subject token for a cloud token.
type gcpTrader struct {
	http *http.Client
	// check is googleURL; a test's fake STS and IAM are let through only
	// by a check this package's tests set (Runtime.federationCheck)
	check func(raw, host string) (*url.URL, error)
	// had hears each token as it is had, before it is sent anywhere
	had func(name, value string)
}

// noRedirect refuses every redirect: a token never follows one.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// trade is the cloud token and when it ends: STS, then generateAccessToken
// when the target names a service account.
func (g *gcpTrader) trade(ctx context.Context, subject string, ex gcpExchange) (string, time.Time, error) {
	sts, err := g.check(ex.STSURL, googleSTSHost)
	if err != nil {
		return "", time.Time{}, &cloudError{Step: federationGCPSTS, Code: "invalid_exchange", Message: err.Error()}
	}
	var iam *url.URL
	if ex.ServiceAccount != "" {
		if iam, err = g.check(ex.IAMCredentialsURL, googleIAMHost); err != nil {
			return "", time.Time{}, &cloudError{Step: federationGCPIAM, Code: "invalid_exchange", Message: err.Error()}
		}
	}
	scopes := ex.Scopes
	if len(scopes) == 0 {
		scopes = []string{googleScope}
	}
	stsScope := scopes
	if iam != nil {
		stsScope = []string{googleScope}
	}
	subjectType := ex.SubjectTokenType
	if subjectType == "" {
		subjectType = jwtTokenType
	}
	form := url.Values{
		"grant_type":           {tokenExchange},
		"audience":             {ex.Audience},
		"scope":                {strings.Join(stsScope, " ")},
		"requested_token_type": {accessTokenType},
		"subject_token_type":   {subjectType},
		"subject_token":        {subject},
	}
	var stsOut struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := g.do(ctx, federationGCPSTS, sts.String(), "application/x-www-form-urlencoded", "", []byte(form.Encode()), &stsOut, subject); err != nil {
		return "", time.Time{}, err
	}
	if stsOut.AccessToken == "" {
		return "", time.Time{}, &cloudError{Step: federationGCPSTS, Status: http.StatusBadGateway, Message: "STS answered no access_token"}
	}
	g.had("sts_token", stsOut.AccessToken)
	if iam == nil {
		life := time.Duration(stsOut.ExpiresIn) * time.Second
		if life <= 0 {
			life = googleDefaultLifetime
		}
		return stsOut.AccessToken, time.Now().Add(life), nil
	}
	life := ex.Duration
	if life <= 0 {
		life = int64(googleDefaultLifetime / time.Second)
	}
	body, _ := json.Marshal(map[string]any{"scope": scopes, "lifetime": strconv.FormatInt(life, 10) + "s"})
	target := strings.TrimRight(iam.String(), "/") + "/projects/-/serviceAccounts/" + url.PathEscape(ex.ServiceAccount) + ":generateAccessToken"
	var iamOut struct {
		AccessToken string    `json:"accessToken"`
		ExpireTime  time.Time `json:"expireTime"`
	}
	if err := g.do(ctx, federationGCPIAM, target, "application/json", "Bearer "+stsOut.AccessToken, body, &iamOut, subject, stsOut.AccessToken); err != nil {
		return "", time.Time{}, err
	}
	if iamOut.AccessToken == "" {
		return "", time.Time{}, &cloudError{Step: federationGCPIAM, Status: http.StatusBadGateway, Message: "generateAccessToken answered no accessToken"}
	}
	g.had("access_token", iamOut.AccessToken)
	expires := iamOut.ExpireTime
	if expires.IsZero() {
		expires = time.Now().Add(time.Duration(life) * time.Second)
	}
	return iamOut.AccessToken, expires, nil
}

// do sends one trade request and decodes its answer; a refusal is a
// *cloudError of Google's error, with every token it was sent scrubbed.
func (g *gcpTrader) do(ctx context.Context, step, rawURL, contentType, authorization string, body []byte, out any, tokens ...string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return &cloudError{Step: step, Unreachable: true, Message: scrub(err.Error(), tokens...)}
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	hc := *g.http
	hc.CheckRedirect = noRedirect
	resp, err := hc.Do(req)
	if err != nil {
		return &cloudError{Step: step, Unreachable: true, Message: scrub(err.Error(), tokens...)}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		ce := &cloudError{Step: step, Status: resp.StatusCode}
		ce.Code, ce.Message = googleError(raw)
		ce.Code, ce.Message = scrub(ce.Code, tokens...), scrub(ce.Message, tokens...)
		// Google's 5xx is the cloud not serving, as unreachable
		ce.Unreachable = resp.StatusCode >= 500
		if ce.Unreachable && ce.Message == "" {
			ce.Message = strconv.Itoa(resp.StatusCode)
		}
		return ce
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return &cloudError{Step: step, Status: http.StatusBadGateway, Message: "the answer is not JSON"}
	}
	return nil
}

// googleError reads Google's refusal: STS's OAuth {error,
// error_description} or IAM's {error: {status, message}}.
func googleError(raw []byte) (code, message string) {
	var e struct {
		Error            json.RawMessage `json:"error"`
		ErrorDescription string          `json:"error_description"`
	}
	if json.Unmarshal(raw, &e) != nil {
		if s := strings.TrimSpace(string(raw)); len(s) < 300 {
			return "", s
		}
		return "", ""
	}
	var s string
	if json.Unmarshal(e.Error, &s) == nil {
		return s, e.ErrorDescription
	}
	var g struct {
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	if json.Unmarshal(e.Error, &g) == nil {
		return g.Status, g.Message
	}
	return "", e.ErrorDescription
}

// heldFederation is one federation target's cloud token as the lease's
// relay holds it: an egress.InjectSource.
type heldFederation struct {
	ref, purpose string
	mint         func(ctx context.Context, ref, purpose string) (*federationToken, error)
	trader       gcpTrader
	now          func() time.Time
	// held hears every token as it is had, before it is used: the
	// lease's redactor (gap 28 item 5, gap 32 item 6)
	held func(ref string, c *egress.Credential)
	logf func(string, ...any)

	mu      sync.Mutex
	cred    *egress.Credential
	lastErr error
	dropped bool
}

// federationSpec is the rule a gcp target's token is written by.
func federationSpec() egress.InjectSpec {
	return egress.InjectSpec{Hosts: []string{googleAPIs}, Form: egress.FormBearer, Field: "access_token",
		Env: slices.Clone(googleTokenEnv), Overwrite: true}
}

// Spec is the rule the token is written by.
func (h *heldFederation) Spec() egress.InjectSpec { return federationSpec() }

// failure is why the last mint or trade failed, nil when it did not.
func (h *heldFederation) failure() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastErr
}

// drop forgets the token at the lease's end: nothing is kept past it.
func (h *heldFederation) drop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cred, h.dropped = nil, true
}

// Credential is the cloud token of the moment, minted and traded again
// within federationRefreshBefore of its end.
func (h *heldFederation) Credential(ctx context.Context) (*egress.Credential, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.dropped {
		return nil, &egress.Refusal{Status: http.StatusBadGateway, Code: egress.CodeUnavailable, Message: "the lease's federation token for " + h.ref + " is gone with the lease"}
	}
	if h.cred != nil && h.now().Before(h.cred.ExpiresAt.Add(-federationRefreshBefore)) {
		return h.cred, nil
	}
	// a token near its end is dropped whether or not another is had
	h.cred = nil
	c, err := h.mintAndTrade(ctx)
	if err != nil {
		h.lastErr = err
		h.logf("nodev1: federation target %s (purpose %s) could not be had: %s", h.ref, h.purpose, err)
		return nil, federationRefusal(h.ref, err)
	}
	h.lastErr, h.cred = nil, c
	return c, nil
}

// mintAndTrade mints the subject token and trades it at the cloud.
func (h *heldFederation) mintAndTrade(ctx context.Context) (*egress.Credential, error) {
	tok, err := h.mint(ctx, h.ref, h.purpose)
	if err != nil {
		return nil, err
	}
	spec := federationSpec()
	had := func(name, value string) {
		if h.held != nil {
			h.held(h.ref, &egress.Credential{Ref: h.ref, Spec: spec, Fields: map[string]string{name: value}})
		}
	}
	had("subject_token", tok.SubjectToken)
	token, expires, err := h.tradeFor(ctx, tok, had)
	if err != nil {
		return nil, err
	}
	return &egress.Credential{Ref: h.ref, Spec: spec, Fields: map[string]string{"access_token": token}, ExpiresAt: expires}, nil
}

// tradeFor trades the subject token at its cloud: the seam AWS and
// Azure join at when a kind needs them.
func (h *heldFederation) tradeFor(ctx context.Context, tok *federationToken, had func(name, value string)) (string, time.Time, error) {
	switch strings.ToLower(tok.Cloud) {
	case "gcp":
		var ex gcpExchange
		if err := json.Unmarshal(tok.Exchange, &ex); err != nil {
			return "", time.Time{}, &cloudError{Step: federationGCPSTS, Status: http.StatusBadGateway, Message: "Latchkey's exchange is not a gcp exchange"}
		}
		t := h.trader
		t.had = had
		return t.trade(ctx, tok.SubjectToken, ex)
	}
	return "", time.Time{}, &unsupportedCloud{cloud: tok.Cloud}
}

// federationRefusal is how a federation token that could not be had
// refuses a request: credential_refused with Latchkey's code, or gcp_sts
// / gcp_iam and Google's error; credential_unavailable when Latchkey or
// the cloud could not be reached.
func federationRefusal(ref string, err error) *egress.Refusal {
	var le *latchkeyError
	var ce *cloudError
	var uc *unsupportedCloud
	switch {
	case errors.As(err, &le) && le.Code != "" && le.Status != http.StatusServiceUnavailable && le.Status != http.StatusUnauthorized:
		return &egress.Refusal{Status: http.StatusBadGateway, Code: egress.CodeRefused,
			Message: fmt.Sprintf("Latchkey refused federation target %s: %s (%d)", ref, le.Code, le.Status)}
	case errors.As(err, &ce) && !ce.Unreachable:
		return &egress.Refusal{Status: http.StatusBadGateway, Code: egress.CodeRefused,
			Message: fmt.Sprintf("federation target %s: %s", ref, ce.Error())}
	case errors.As(err, &uc):
		return &egress.Refusal{Status: http.StatusBadGateway, Code: egress.CodeRefused,
			Message: fmt.Sprintf("federation target %s: %s", ref, uc.Error())}
	}
	return &egress.Refusal{Status: http.StatusBadGateway, Code: egress.CodeUnavailable,
		Message: "the federation token for " + ref + " could not be had"}
}

// federationFailed is ref's token not had as the lease opens: the
// product's configuration when Latchkey refused it (403
// purpose_not_granted or not_the_jobs, 404 target_not_found, 409
// target_disabled), the cloud refused the target's trust, or the target's
// cloud is one no node trades yet; else the infrastructure's (gap 14).
// The reason names credential_refused or credential_unavailable as the
// request would have, and never a token.
func federationFailed(ref string, err error) error {
	ce := &credentialError{err: fmt.Errorf("the federation target %s the job's access names: %s: %w", ref, federationRefusal(ref, err).Code, err)}
	var le *latchkeyError
	var cl *cloudError
	var uc *unsupportedCloud
	switch {
	case errors.As(err, &le):
		ce.product = le.productsConfig()
	case errors.As(err, &cl):
		ce.product = cl.productsConfig()
	case errors.As(err, &uc):
		ce.product = true
	}
	return ce
}

// federationPurpose is ref's purpose: the one the offer named, else the
// kind's name (its last segment), as Latchkey reads a job's kind.
func federationPurpose(f meshapi.FederationRef, kind string) string {
	if p := strings.ToLower(strings.TrimSpace(f.Purpose)); p != "" {
		return p
	}
	if i := strings.LastIndex(kind, "/"); i >= 0 {
		kind = kind[i+1:]
	}
	return strings.ToLower(strings.TrimSpace(kind))
}

// federates says the brief carries federation targets.
func federates(b *meshapi.Brief) bool { return b.Access != nil && len(b.Access.Federation) > 0 }

// federationRules mints and trades each federation target of the brief
// as the lease opens and answers its rule over the hosts of allowed under
// *.googleapis.com, the placeholders the cloud's tools read, and every
// target held. A host an earlier target's rule writes is not another's:
// two targets on one host would race for its Authorization. A target
// whose hosts the egress names none of is not minted, which is said: it
// never widens egress.
func (r *Runtime) federationRules(ctx context.Context, b *meshapi.Brief, tokens *sessionTokens, allowed *egress.Policy, held func(string, *egress.Credential)) ([]egress.Rule, []string, []*heldFederation, error) {
	var (
		rules []egress.Rule
		all   []*heldFederation
		taken = map[string]string{}
	)
	hc := r.federationHTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	check := r.federationCheck
	if check == nil {
		check = googleURL
	}
	spec := federationSpec()
	for _, f := range b.Access.Federation {
		ref := strings.TrimSpace(f.Ref)
		if ref == "" {
			continue
		}
		purpose := federationPurpose(f, b.Kind)
		var hosts []string
		for _, host := range b.Egress.Hosts {
			host = strings.ToLower(strings.TrimSuffix(host, "."))
			if strings.HasPrefix(host, "*.") || !spec.MatchesHost(host) {
				continue
			}
			if ok, _, _ := allowed.Allow(host, egress.Port); !ok {
				continue
			}
			if other, ok := taken[host]; ok {
				r.logf("nodev1: lease %s: %s is written by federation target %s; not by %s too", b.LeaseID, host, other, ref)
				continue
			}
			hosts = append(hosts, host)
		}
		if len(hosts) == 0 {
			r.logf("nodev1: lease %s: the kind's egress hosts name none of federation target %s's (%s); it is not minted", b.LeaseID, ref, googleAPIs)
			continue
		}
		h := &heldFederation{ref: ref, purpose: purpose, mint: tokens.FederationToken, now: time.Now, held: held, logf: r.logf,
			trader: gcpTrader{http: hc, check: check}}
		if _, err := h.Credential(ctx); err != nil {
			return nil, nil, all, federationFailed(ref, cmpErr(h.failure(), err))
		}
		all = append(all, h)
		for _, host := range hosts {
			taken[host] = ref
		}
		rule := egress.NewInjectRule(ref, federationKind, h, hosts)
		rule.Purpose = purpose
		rules = append(rules, rule)
		r.logf("nodev1: lease %s: injects federation target %s (purpose %s, bearer) on %s", b.LeaseID, ref, purpose, strings.Join(hosts, ", "))
	}
	var env []string
	if len(rules) > 0 {
		for _, name := range googleTokenEnv {
			env = append(env, name+"="+egress.Placeholder)
		}
	}
	return rules, env, all, nil
}
