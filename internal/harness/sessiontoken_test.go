package harness

import (
	"os"
	"path/filepath"
	"testing"
)

// sessionKeeper is a SessionTokenKeeper recording who each file is for.
type sessionKeeper struct {
	rotatingKeeper
	kinds []Kind
	metas []SessionMeta
}

func (k *sessionKeeper) KeepSession(path string, kind Kind, meta SessionMeta) (func(bool), error) {
	k.kinds, k.metas = append(k.kinds, kind), append(k.metas, meta)
	return k.rotatingKeeper.Keep(path)
}

// A job naming a TokenDir has its host token file there — its org's MCP
// root — kept by a SessionTokenKeeper for that job's own session; a pod
// keeps its file in its HOME, still for its own session (ADR-035).
func TestPlaceMeshContextKeepsTheJobsOwnSessionInItsTokenDir(t *testing.T) {
	k := &sessionKeeper{}
	tokenDir := filepath.Join(t.TempDir(), "orgs", "acme", "mcp", "sessions", "item-1")
	if err := os.MkdirAll(tokenDir, 0o700); err != nil {
		t.Fatal(err)
	}
	meta := SessionMeta{NodeID: "n", ItemID: "item", Attempt: 1}
	job := Job{Kind: KindBuild, MCPServers: relayServers(), TokenDir: tokenDir, Meta: meta}
	job, path, release, err := placeMeshContext(job, k, t.TempDir(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer release(true)
	if path != filepath.Join(tokenDir, TokenFileName) || job.tokenFile != path {
		t.Fatalf("token file %q, want it in %s", path, tokenDir)
	}
	if _, tf, _ := relayOf(job.MCPServers[BuiltinMCP]); tf != path {
		t.Fatalf("relay reads %q, want %q", tf, path)
	}
	if len(k.metas) != 1 || k.metas[0].ItemID != "item" || k.metas[0].Attempt != 1 || k.kinds[0] != KindBuild {
		t.Fatalf("kept for %v %+v", k.kinds, k.metas)
	}

	home := t.TempDir()
	job = Job{Kind: KindBuild, MCPServers: relayServers(), TokenDir: tokenDir, Meta: meta}
	_, path, release2, err := placeMeshContext(job, k, home, true, filepath.Join(home, "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer release2(true)
	if filepath.Dir(path) != home || len(k.metas) != 2 {
		t.Fatalf("pod token file %q, want it in the pod's HOME %s", path, home)
	}
}
