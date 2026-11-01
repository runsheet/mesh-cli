package nodev1

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/egress"
	"projectmesh.io/mesh-cli/internal/meshapi"
)

const (
	injAWSKeyID  = "AKIDSIGNINGTEST01"
	injAWSSecret = "aws-secret-access-key-value-0001"
)

// A Secret in the signing form: the sandbox's AWS tool signs on the
// placeholder keys, which the pod holds as AWS_ACCESS_KEY_ID and
// AWS_SECRET_ACCESS_KEY, and the relay signs again with the real ones;
// the secret key is nowhere but in the signature (mesh-specs gap 28 item
// 3, aws_sigv4).
func TestTheRelaySignsWithACarriedAWSKey(t *testing.T) {
	needCurl(t)
	g, sb, _ := podRig(t)
	logs := &logSink{t: t}
	g.rt.Logf = logs.logf
	host := injectHostRig(t, g)
	g.latchkey.setCarried("sec_aws", forJobCredential{
		Inject: egress.InjectSpec{Hosts: []string{"sts.amazonaws.test"}, Form: "signing", Scheme: "aws_sigv4", Region: "us-east-1", Service: "sts"},
		Fields: map[string]string{"access_key_id": injAWSKeyID, "secret_access_key": injAWSSecret}, Version: json.RawMessage("7"), ExpiresAt: time.Now().Add(time.Hour)})
	dir := t.TempDir()
	answers := filepath.Join(dir, "answers")
	dummy := `-H "Authorization: AWS4-HMAC-SHA256 Credential=$AWS_ACCESS_KEY_ID/20250101/us-east-1/sts/aws4_request, SignedHeaders=host, Signature=00"`
	sb.Agent = hostAgent(t, filepath.Join(dir, "claude"), curlScript(answers,
		dummy+` "https://sts.amazonaws.test/?Action=GetCallerIdentity&Version=2011-06-15"`,
		`-H "Authorization: AWS4-HMAC-SHA256 Credential=AKIAOTHER/20250101/us-east-1/sts/aws4_request, SignedHeaders=host, Signature=00" https://sts.amazonaws.test/`,
	))
	g.offer(t, func(b *meshapi.Brief) {
		b.FinishTool, b.FinishSchema = "", nil
		b.Egress = &meshapi.Egress{Hosts: []string{"sts.amazonaws.test"}, MCP: []string{"conform"}, Model: true}
		b.Access = &meshapi.Access{Repo: true, MCP: []string{"conform"}, Model: true, Secrets: []string{"sec_aws"}}
	})
	run(t, g, context.Background())
	got, _ := os.ReadFile(answers)
	lines := strings.Split(strings.TrimSpace(string(got)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], " 200") || !strings.Contains(strings.ToLower(lines[1]), "credential_preset") {
		t.Fatalf("the pod's answers %q", got)
	}
	seen := strings.Join(host.lines(), "\n")
	if !strings.Contains(seen, `auth="AWS4-HMAC-SHA256 Credential=`+injAWSKeyID+`/`) || !strings.Contains(seen, "SignedHeaders=host;x-amz-date") {
		t.Fatalf("the host saw\n%s", seen)
	}
	env := sb.AgentSpecs()[0].Env
	if envValue(env, "AWS_ACCESS_KEY_ID") != egress.Placeholder || envValue(env, "AWS_SECRET_ACCESS_KEY") != egress.Placeholder {
		t.Fatalf("the pod's AWS keys are %q and %q", envValue(env, "AWS_ACCESS_KEY_ID"), envValue(env, "AWS_SECRET_ACCESS_KEY"))
	}
	for where, s := range map[string]string{"the pod's environment": strings.Join(env, "\n"), "the node's log": logs.String(), "the host": seen} {
		if strings.Contains(s, injAWSSecret) {
			t.Fatalf("%s carries the AWS secret key", where)
		}
	}
	var forms []string
	for _, c := range g.fake.Calls("access") {
		var req meshapi.AccessRequest
		json.Unmarshal(c.Body, &req)
		for _, r := range req.Records {
			if strings.HasPrefix(r.Service, "inject ") {
				forms = append(forms, r.Service+" "+r.Outcome+" "+r.Reason)
			}
		}
	}
	if want := "inject signing aws_sigv4 ok 200 version 7\ninject signing aws_sigv4 refused credential_preset"; strings.Join(forms, "\n") != want {
		t.Fatalf("recorded\n%s", strings.Join(forms, "\n"))
	}
}
