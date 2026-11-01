package nodev1

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
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
// The other clouds the same way, each URL checked before a token is sent
// there:
//
//   - aws: AssumeRoleWithWebIdentity at exchange.sts_url (STS's own host,
//     sts[.<region>].amazonaws.com[.cn]) for exchange.role_arn, as
//     exchange.role_session_name, for exchange.duration with
//     exchange.session_policy; the role's keys sign every request to the
//     kind's egress hosts under *.amazonaws.com (gap 28's signing form,
//     aws_sigv4, for each request's own region and service), the pod
//     holding AWS_ACCESS_KEY_ID and
//     AWS_SECRET_ACCESS_KEY as mesh so its SDK or CLI signs with the
//     placeholder the relay replaces;
//   - azure: the subject token as a client assertion at Entra's
//     exchange.token_url (login.microsoftonline.com) for exchange.client_id
//     and exchange.scope; the access token a bearer on exchange.hosts;
//   - oidc: a direct target's subject token is itself the bearer on
//     exchange.hosts, re-minted before its five minutes end; a trading
//     target's is traded by RFC 8693 at exchange.token_url (https, the
//     target's own endpoint) and the access token is the bearer.
//
// A target's cloud and hosts are known once it is minted: the lease's
// first mint says them, and a target whose hosts the kind's egress names
// none of is not traded, which is said.
//
// The subject token, every intermediate token and the cloud credential
// are held in this process's memory alone, each registered with the
// lease's redactor as it is had, before it is used (an AWS key's id is
// not: it is no secret, and S3 echoes it); minted once as the lease
// opens (a refusal hands the lease back: product for a refusal of the
// product's configuration, credential_timeout for Latchkey or the cloud
// unreachable, gap 14), again on the first request within a minute of
// the cloud credential's end, and dropped at the lease's end. Nothing is
// kept across leases.

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
	federationAWSSTS        = "aws_sts"
	federationAzure         = "azure_entra"
	federationOIDC          = "oidc_token"
	federationTargetMissing = "target_not_found"
	federationCloudNotYet   = "cloud_not_supported"
)

// The other clouds' endpoints: what an exchange URL's host must be.
const (
	// awsSTSHost names STS's own hosts, global or regional, in either
	// partition (awsSTSPattern)
	awsSTSHost = "sts.amazonaws.com"
	// azureLoginHost is Entra's public authority
	azureLoginHost = "login.microsoftonline.com"
	// anyHTTPSHost is an oidc target's own token endpoint: any https URL
	anyHTTPSHost = ""
	// awsAPIs and awsAPIsCN are the hosts an aws target's keys sign on
	awsAPIs   = "*.amazonaws.com"
	awsAPIsCN = "*.amazonaws.com.cn"
	// awsSTSVersion is the STS API version assumed at
	awsSTSVersion = "2011-06-15"
	// clientAssertionJWT is RFC 7523's client assertion type
	clientAssertionJWT = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"
	// subjectDefaultLifetime is a subject token's life when Latchkey says
	// none, and a traded oidc token's when its endpoint says none
	subjectDefaultLifetime = 5 * time.Minute
	// pendingFresh is how long a subject token minted as the lease opens
	// is traded rather than minted again
	pendingFresh = 3 * time.Minute
)

// awsSTSPattern is STS's hosts: sts.amazonaws.com and
// sts.<region>.amazonaws.com, and their .cn partition's
var awsSTSPattern = regexp.MustCompile(`^sts(\.[a-z]{2}(-[a-z]+)+-[0-9]+)?\.amazonaws\.com(\.cn)?$`)

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
	return fmt.Sprintf("%s: this node trades gcp, aws, azure and oidc federation targets, not %q", federationCloudNotYet, e.cloud)
}

// googleURL is raw when it is https on host exactly, with no user, no
// port but 443 and no fragment: Google's endpoint, and nowhere else a
// token goes.
func googleURL(raw, host string) (*url.URL, error) {
	return exactURL(raw, host, "Google's")
}

// exactURL is raw when it is https on host exactly, with no user, no
// port but 443 and no fragment; whose names whose host it is.
func exactURL(raw, host, whose string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("the exchange's url %q is not a url", raw)
	}
	if u.Scheme != "https" || u.User != nil || u.Fragment != "" || !strings.EqualFold(u.Hostname(), host) || (u.Port() != "" && u.Port() != "443") {
		return nil, fmt.Errorf("the exchange's url %q is not %s %s on https; no token is sent there", raw, whose, host)
	}
	return u, nil
}

// cloudURL checks an exchange URL against the endpoint it must be: one
// of Google's or Entra's exactly, STS's own hosts for AWS, any https URL
// with no user or fragment for an oidc target's own endpoint.
func cloudURL(raw, want string) (*url.URL, error) {
	switch want {
	case awsSTSHost:
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") ||
			!awsSTSPattern.MatchString(strings.ToLower(u.Hostname())) {
			return nil, fmt.Errorf("the exchange's url %q is not AWS STS's on https; no token is sent there", raw)
		}
		return u, nil
	case azureLoginHost:
		return exactURL(raw, azureLoginHost, "Entra's")
	case anyHTTPSHost:
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
			return nil, fmt.Errorf("the exchange's url %q is not an https URL; no token is sent there", raw)
		}
		return u, nil
	}
	return googleURL(raw, want)
}

// cloudTrader trades a target's subject token at its cloud.
type cloudTrader struct {
	http *http.Client
	// check is cloudURL; a test's fake clouds are let through only by a
	// check this package's tests set (Runtime.federationCheck)
	check func(raw, host string) (*url.URL, error)
	// had hears each token as it is had, before it is sent anywhere
	had func(name, value string)
}

// noRedirect refuses every redirect: a token never follows one.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// tradeGCP is the cloud token and when it ends: STS, then generateAccessToken
// when the target names a service account.
func (g *cloudTrader) tradeGCP(ctx context.Context, subject string, ex gcpExchange) (string, time.Time, error) {
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
	if err := g.do(ctx, federationGCPSTS, sts.String(), formType, "", []byte(form.Encode()), &stsOut, subject); err != nil {
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

// formType is a form POST's content type.
const formType = "application/x-www-form-urlencoded"

// do sends one trade request and decodes its answer, JSON or (AWS STS's)
// XML; a refusal is a *cloudError of the cloud's error, with every token
// it was sent scrubbed.
func (g *cloudTrader) do(ctx context.Context, step, rawURL, contentType, authorization string, body []byte, out any, tokens ...string) error {
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
		ce.Code, ce.Message = cloudErrorBody(raw)
		ce.Code, ce.Message = scrub(ce.Code, tokens...), scrub(ce.Message, tokens...)
		// Google's 5xx is the cloud not serving, as unreachable
		ce.Unreachable = resp.StatusCode >= 500
		if ce.Unreachable && ce.Message == "" {
			ce.Message = strconv.Itoa(resp.StatusCode)
		}
		return ce
	}
	if t := bytes.TrimSpace(raw); len(t) > 0 && t[0] == '<' {
		if err := xml.Unmarshal(t, out); err != nil {
			return &cloudError{Step: step, Status: http.StatusBadGateway, Message: "the answer is not XML it reads"}
		}
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return &cloudError{Step: step, Status: http.StatusBadGateway, Message: "the answer is not JSON"}
	}
	return nil
}

// cloudErrorBody reads a cloud's refusal: AWS's XML
// <ErrorResponse><Error><Code/><Message/>, else googleError's shapes
// (which an OAuth endpoint's {error, error_description} is one of).
func cloudErrorBody(raw []byte) (code, message string) {
	if t := bytes.TrimSpace(raw); len(t) > 0 && t[0] == '<' {
		var e struct {
			Error struct {
				Code    string `xml:"Code"`
				Message string `xml:"Message"`
			} `xml:"Error"`
		}
		if xml.Unmarshal(t, &e) == nil {
			return e.Error.Code, e.Error.Message
		}
		return "", ""
	}
	return googleError(raw)
}

// awsExchange is an aws target's exchange, as Latchkey answers it.
type awsExchange struct {
	STSURL          string `json:"sts_url"`
	Version         string `json:"version"`
	RoleARN         string `json:"role_arn"`
	RoleSessionName string `json:"role_session_name"`
	Region          string `json:"region"`
	SessionPolicy   string `json:"session_policy"`
	// Duration is the role session's life in seconds
	Duration int64 `json:"duration"`
}

// awsAssumed is AssumeRoleWithWebIdentity's answer.
type awsAssumed struct {
	Result struct {
		Credentials struct {
			AccessKeyID     string    `xml:"AccessKeyId"`
			SecretAccessKey string    `xml:"SecretAccessKey"`
			SessionToken    string    `xml:"SessionToken"`
			Expiration      time.Time `xml:"Expiration"`
		} `xml:"Credentials"`
	} `xml:"AssumeRoleWithWebIdentityResult"`
}

// tradeAWS assumes the target's role with the subject token: the role's
// keys and when they end.
func (g *cloudTrader) tradeAWS(ctx context.Context, subject string, ex awsExchange) (map[string]string, time.Time, error) {
	sts, err := g.check(ex.STSURL, awsSTSHost)
	if err != nil {
		return nil, time.Time{}, &cloudError{Step: federationAWSSTS, Code: "invalid_exchange", Message: err.Error()}
	}
	form := url.Values{
		"Action":           {"AssumeRoleWithWebIdentity"},
		"Version":          {cmp.Or(ex.Version, awsSTSVersion)},
		"RoleArn":          {ex.RoleARN},
		"RoleSessionName":  {cmp.Or(ex.RoleSessionName, "mesh")},
		"WebIdentityToken": {subject},
	}
	if ex.Duration > 0 {
		form.Set("DurationSeconds", strconv.FormatInt(ex.Duration, 10))
	}
	if ex.SessionPolicy != "" {
		form.Set("Policy", ex.SessionPolicy)
	}
	var out awsAssumed
	if err := g.do(ctx, federationAWSSTS, sts.String(), formType, "", []byte(form.Encode()), &out, subject); err != nil {
		return nil, time.Time{}, err
	}
	c := out.Result.Credentials
	if c.AccessKeyID == "" || c.SecretAccessKey == "" || c.SessionToken == "" {
		return nil, time.Time{}, &cloudError{Step: federationAWSSTS, Status: http.StatusBadGateway, Message: "STS answered no credentials"}
	}
	g.had("secret_access_key", c.SecretAccessKey)
	g.had("session_token", c.SessionToken)
	expires := c.Expiration
	if expires.IsZero() {
		expires = time.Now().Add(time.Duration(cmp.Or(ex.Duration, 3600)) * time.Second)
	}
	return map[string]string{"access_key_id": c.AccessKeyID, "secret_access_key": c.SecretAccessKey, "session_token": c.SessionToken}, expires, nil
}

// azureExchange is an azure target's exchange, as Latchkey answers it.
type azureExchange struct {
	TokenURL            string   `json:"token_url"`
	GrantType           string   `json:"grant_type"`
	ClientID            string   `json:"client_id"`
	Scope               string   `json:"scope"`
	ClientAssertionType string   `json:"client_assertion_type"`
	Hosts               []string `json:"hosts"`
}

// oauthToken is an OAuth token endpoint's answer.
type oauthToken struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int64  `json:"expires_in"`
}

// tradeAzure trades the subject token at Entra as the app's client
// assertion: the access token and when it ends.
func (g *cloudTrader) tradeAzure(ctx context.Context, subject string, ex azureExchange) (map[string]string, time.Time, error) {
	u, err := g.check(ex.TokenURL, azureLoginHost)
	if err != nil {
		return nil, time.Time{}, &cloudError{Step: federationAzure, Code: "invalid_exchange", Message: err.Error()}
	}
	form := url.Values{
		"grant_type":            {cmp.Or(ex.GrantType, "client_credentials")},
		"client_id":             {ex.ClientID},
		"scope":                 {ex.Scope},
		"client_assertion_type": {cmp.Or(ex.ClientAssertionType, clientAssertionJWT)},
		"client_assertion":      {subject},
	}
	return g.oauth(ctx, federationAzure, u.String(), form, time.Hour, subject)
}

// oidcExchange is an oidc target's exchange, as Latchkey answers it.
type oidcExchange struct {
	Mode               string   `json:"mode"`
	TokenURL           string   `json:"token_url"`
	GrantType          string   `json:"grant_type"`
	Audience           string   `json:"audience"`
	SubjectTokenType   string   `json:"subject_token_type"`
	RequestedTokenType string   `json:"requested_token_type"`
	Hosts              []string `json:"hosts"`
}

// direct says the target takes the subject token itself.
func (ex oidcExchange) direct() bool { return ex.TokenURL == "" || ex.Mode == "direct" }

// tradeOIDC trades the subject token at the target's own endpoint by
// RFC 8693: the access token and when it ends.
func (g *cloudTrader) tradeOIDC(ctx context.Context, subject string, ex oidcExchange) (map[string]string, time.Time, error) {
	u, err := g.check(ex.TokenURL, anyHTTPSHost)
	if err != nil {
		return nil, time.Time{}, &cloudError{Step: federationOIDC, Code: "invalid_exchange", Message: err.Error()}
	}
	form := url.Values{
		"grant_type":           {cmp.Or(ex.GrantType, tokenExchange)},
		"subject_token_type":   {cmp.Or(ex.SubjectTokenType, jwtTokenType)},
		"requested_token_type": {cmp.Or(ex.RequestedTokenType, accessTokenType)},
		"subject_token":        {subject},
	}
	if ex.Audience != "" {
		form.Set("audience", ex.Audience)
	}
	return g.oauth(ctx, federationOIDC, u.String(), form, subjectDefaultLifetime, subject)
}

// oauth posts form to an OAuth token endpoint: the access token and
// when it ends (life when the answer says not).
func (g *cloudTrader) oauth(ctx context.Context, step, rawURL string, form url.Values, life time.Duration, subject string) (map[string]string, time.Time, error) {
	var out oauthToken
	if err := g.do(ctx, step, rawURL, formType, "", []byte(form.Encode()), &out, subject); err != nil {
		return nil, time.Time{}, err
	}
	if out.AccessToken == "" {
		return nil, time.Time{}, &cloudError{Step: step, Status: http.StatusBadGateway, Message: "the endpoint answered no access_token"}
	}
	g.had("access_token", out.AccessToken)
	if out.ExpiresIn > 0 {
		life = time.Duration(out.ExpiresIn) * time.Second
	}
	return map[string]string{"access_token": out.AccessToken}, time.Now().Add(life), nil
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

// heldFederation is one federation target's cloud credential as the
// lease's relay holds it: an egress.InjectSource.
type heldFederation struct {
	ref, purpose string
	mint         func(ctx context.Context, ref, purpose string) (*federationToken, error)
	trader       cloudTrader
	now          func() time.Time
	// held hears every token as it is had, before it is used: the
	// lease's redactor (gap 28 item 5, gap 32 item 6)
	held func(ref string, c *egress.Credential)
	logf func(string, ...any)

	mu sync.Mutex
	// spec is the rule the target's cloud writes by, known from its
	// first mint; opened says it is
	spec   egress.InjectSpec
	opened bool
	// pending is the subject token the lease's opening minted, traded by
	// the first Credential while it is fresh
	pending   *federationToken
	pendingAt time.Time
	cred      *egress.Credential
	lastErr   error
	dropped   bool
}

// federationSpec is the rule a gcp target's token is written by.
func federationSpec() egress.InjectSpec {
	return egress.InjectSpec{Hosts: []string{googleAPIs}, Form: egress.FormBearer, Field: "access_token",
		Env: slices.Clone(googleTokenEnv), Overwrite: true}
}

// federationSpecFor is the rule a minted target's cloud credential is
// written by: gcp's bearer on Google's APIs; aws's signing on its
// partition's; azure's and oidc's bearer on the hosts Latchkey names for
// the target. Every cloud but aws's overwrites the Authorization its
// tools send; aws's replaces only the placeholder's signature.
func federationSpecFor(tok *federationToken) (egress.InjectSpec, error) {
	switch strings.ToLower(tok.Cloud) {
	case "gcp":
		return federationSpec(), nil
	case "aws":
		var ex awsExchange
		if err := json.Unmarshal(tok.Exchange, &ex); err != nil {
			return egress.InjectSpec{}, &cloudError{Step: federationAWSSTS, Status: http.StatusBadGateway, Message: "Latchkey's exchange is not an aws exchange"}
		}
		hosts := awsAPIs
		if u, err := url.Parse(ex.STSURL); err == nil && strings.HasSuffix(strings.ToLower(u.Hostname()), ".amazonaws.com.cn") {
			hosts = awsAPIsCN
		}
		// no region or service: each request's own (egress.signSigV4)
		return egress.InjectSpec{Hosts: []string{hosts}, Form: egress.FormSigning, Scheme: egress.SchemeSigV4}, nil
	case "azure", "oidc":
		var ex struct {
			Hosts []string `json:"hosts"`
		}
		step := federationAzure
		if strings.EqualFold(tok.Cloud, "oidc") {
			step = federationOIDC
		}
		if err := json.Unmarshal(tok.Exchange, &ex); err != nil || len(ex.Hosts) == 0 {
			return egress.InjectSpec{}, &cloudError{Step: step, Status: http.StatusBadRequest, Code: "invalid_exchange",
				Message: "Latchkey's exchange names no hosts to write the token on"}
		}
		spec := egress.InjectSpec{Hosts: ex.Hosts, Form: egress.FormBearer, Field: "access_token", Overwrite: true}
		if why := spec.Check(); why != "" {
			return egress.InjectSpec{}, &cloudError{Step: step, Status: http.StatusBadRequest, Code: "invalid_exchange", Message: why}
		}
		return spec, nil
	}
	return egress.InjectSpec{}, &unsupportedCloud{cloud: tok.Cloud}
}

// open mints the target's first subject token, which says its cloud and
// so the rule it is written by; the token is kept for the first trade.
func (h *heldFederation) open(ctx context.Context) (egress.InjectSpec, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	tok, err := h.mint(ctx, h.ref, h.purpose)
	if err == nil {
		h.heard("subject_token", tok.SubjectToken)
		h.spec, err = federationSpecFor(tok)
	}
	if err != nil {
		h.lastErr = err
		return egress.InjectSpec{}, err
	}
	h.opened, h.pending, h.pendingAt = true, tok, h.now()
	return h.spec, nil
}

// heard hands one token to the redactor before it is used.
func (h *heldFederation) heard(name, value string) {
	if h.held != nil {
		h.held(h.ref, &egress.Credential{Ref: h.ref, Spec: h.spec, Fields: map[string]string{name: value}})
	}
}

// Spec is the rule the credential is written by.
func (h *heldFederation) Spec() egress.InjectSpec {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.opened {
		return federationSpec()
	}
	return h.spec
}

// failure is why the last mint or trade failed, nil when it did not.
func (h *heldFederation) failure() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastErr
}

// drop forgets the credential at the lease's end: nothing is kept past
// it.
func (h *heldFederation) drop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cred, h.pending, h.dropped = nil, nil, true
}

// Credential is the cloud credential of the moment, minted and traded
// again within federationRefreshBefore of its end.
func (h *heldFederation) Credential(ctx context.Context) (*egress.Credential, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.dropped {
		return nil, &egress.Refusal{Status: http.StatusBadGateway, Code: egress.CodeUnavailable, Message: "the lease's federation token for " + h.ref + " is gone with the lease"}
	}
	if h.cred != nil && h.now().Before(h.cred.ExpiresAt.Add(-federationRefreshBefore)) {
		return h.cred, nil
	}
	// a credential near its end is dropped whether or not another is had
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

// mintAndTrade mints the subject token (or takes the one the lease's
// opening minted, while fresh) and trades it at the cloud.
func (h *heldFederation) mintAndTrade(ctx context.Context) (*egress.Credential, error) {
	tok := h.pending
	h.pending = nil
	if tok == nil || h.now().Sub(h.pendingAt) > pendingFresh {
		var err error
		if tok, err = h.mint(ctx, h.ref, h.purpose); err != nil {
			return nil, err
		}
		if !h.opened {
			if h.spec, err = federationSpecFor(tok); err != nil {
				return nil, err
			}
			h.opened = true
		}
		h.heard("subject_token", tok.SubjectToken)
	}
	fields, expires, err := h.tradeFor(ctx, tok)
	if err != nil {
		return nil, err
	}
	return &egress.Credential{Ref: h.ref, Spec: h.spec, Fields: fields, ExpiresAt: expires}, nil
}

// tradeFor trades the subject token at its cloud: the credential's
// fields and when it ends.
func (h *heldFederation) tradeFor(ctx context.Context, tok *federationToken) (map[string]string, time.Time, error) {
	t := h.trader
	t.had = h.heard
	bad := func(step, cloud string) error {
		return &cloudError{Step: step, Status: http.StatusBadGateway, Message: "Latchkey's exchange is not a " + cloud + " exchange"}
	}
	switch strings.ToLower(tok.Cloud) {
	case "gcp":
		var ex gcpExchange
		if err := json.Unmarshal(tok.Exchange, &ex); err != nil {
			return nil, time.Time{}, bad(federationGCPSTS, "gcp")
		}
		token, expires, err := t.tradeGCP(ctx, tok.SubjectToken, ex)
		if err != nil {
			return nil, time.Time{}, err
		}
		return map[string]string{"access_token": token}, expires, nil
	case "aws":
		var ex awsExchange
		if err := json.Unmarshal(tok.Exchange, &ex); err != nil {
			return nil, time.Time{}, bad(federationAWSSTS, "aws")
		}
		return t.tradeAWS(ctx, tok.SubjectToken, ex)
	case "azure":
		var ex azureExchange
		if err := json.Unmarshal(tok.Exchange, &ex); err != nil {
			return nil, time.Time{}, bad(federationAzure, "azure")
		}
		return t.tradeAzure(ctx, tok.SubjectToken, ex)
	case "oidc":
		var ex oidcExchange
		if err := json.Unmarshal(tok.Exchange, &ex); err != nil {
			return nil, time.Time{}, bad(federationOIDC, "oidc")
		}
		if ex.direct() {
			// the audience takes the subject token itself
			life := time.Duration(tok.ExpiresIn) * time.Second
			if life <= 0 {
				life = subjectDefaultLifetime
			}
			return map[string]string{"access_token": tok.SubjectToken}, h.now().Add(life), nil
		}
		return t.tradeOIDC(ctx, tok.SubjectToken, ex)
	}
	return nil, time.Time{}, &unsupportedCloud{cloud: tok.Cloud}
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
// as the lease opens and answers its rule over the kind's egress hosts
// that its cloud's hosts name, the placeholders the cloud's tools read,
// and every target held. A host an earlier target's rule writes is not
// another's: two targets on one host would race for its Authorization.
// A target whose hosts the egress names none of is minted (its cloud is
// known only then) but not traded, which is said: it never widens
// egress.
func (r *Runtime) federationRules(ctx context.Context, b *meshapi.Brief, tokens *sessionTokens, allowed *egress.Policy, held func(string, *egress.Credential)) ([]egress.Rule, []string, []*heldFederation, error) {
	var (
		rules []egress.Rule
		all   []*heldFederation
		env   []string
		taken = map[string]string{}
	)
	hc := r.federationHTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	check := r.federationCheck
	if check == nil {
		check = cloudURL
	}
	for _, f := range b.Access.Federation {
		ref := strings.TrimSpace(f.Ref)
		if ref == "" {
			continue
		}
		purpose := federationPurpose(f, b.Kind)
		h := &heldFederation{ref: ref, purpose: purpose, mint: tokens.FederationToken, now: time.Now, held: held, logf: r.logf,
			trader: cloudTrader{http: hc, check: check}}
		spec, err := h.open(ctx)
		if err != nil {
			return nil, nil, all, federationFailed(ref, err)
		}
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
			h.drop()
			r.logf("nodev1: lease %s: the kind's egress hosts name none of federation target %s's (%s); it is not traded", b.LeaseID, ref, strings.Join(spec.Hosts, ", "))
			continue
		}
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
		for _, name := range append(slices.Clone(spec.Env), signingEnv(spec)...) {
			if v := name + "=" + egress.Placeholder; !slices.Contains(env, v) {
				env = append(env, v)
			}
		}
		form := spec.FormOf()
		if spec.Scheme != "" {
			form += " " + spec.Scheme
		}
		r.logf("nodev1: lease %s: injects federation target %s (purpose %s, %s) on %s", b.LeaseID, ref, purpose, form, strings.Join(hosts, ", "))
	}
	slices.Sort(env)
	return rules, env, all, nil
}
