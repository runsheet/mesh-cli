package sandbox

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// The node image is asked for its codex once, in a throwaway container
// of the configured image, and its answer read as `codex <version>`.
func TestCodexVersionAsksTheImage(t *testing.T) {
	var asked []string
	rt := &Fake{Image: func(image string, cmd []string) (string, error) {
		asked = append([]string{image}, cmd...)
		return "codex-cli 0.157.0\n", nil
	}}
	sb := &Sandbox{Runtime: rt, Config: Config{Image: "ghcr.io/runsheet/mesh-node:v1.2.3"}}
	got, err := sb.CodexVersion(context.Background())
	if err != nil || got != "codex 0.157.0" {
		t.Fatalf("CodexVersion = %q, %v; want codex 0.157.0", got, err)
	}
	if want := []string{"ghcr.io/runsheet/mesh-node:v1.2.3", "codex", "--version"}; !reflect.DeepEqual(asked, want) {
		t.Fatalf("asked %v; want %v", asked, want)
	}
}

// An image without codex is an error, as is no sandbox at all.
func TestCodexVersionWithoutCodex(t *testing.T) {
	sb := &Sandbox{Runtime: &Fake{}, Config: Config{Image: "img"}}
	if _, err := sb.CodexVersion(context.Background()); err == nil {
		t.Fatal("an image without codex answered a version")
	}
	sb.Runtime = &Fake{Image: func(string, []string) (string, error) { return "", errors.New("exit 127") }}
	if _, err := sb.CodexVersion(context.Background()); err == nil || !strings.Contains(err.Error(), "127") {
		t.Fatalf("err = %v; want the runtime's", err)
	}
	var none *Sandbox
	if _, err := none.CodexVersion(context.Background()); err == nil {
		t.Fatal("no sandbox answered a version")
	}
}

// ImageExec's podman arguments: removed at its end, no network.
func TestImageExecArgs(t *testing.T) {
	got := ImageExecArgs("img:1", []string{"codex", "--version"})
	want := []string{"run", "--rm", "--network", "none", "img:1", "codex", "--version"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ImageExecArgs = %v; want %v", got, want)
	}
}
