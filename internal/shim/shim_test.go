package shim

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/ipc"
)

func nopLogf(_ string, _ ...any) {}

// --- ParseMeshSendAttachments validation tests ---

func TestParseMeshSendAttachments(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	// media.ValidateAttachmentPath still allow-lists ~/.mesh/whatsapp-accounts/
	// (we did not touch internal/media/ in Plan 00-05); use it here as a
	// stable per-agent media dir for these isolation tests.
	meshBase := filepath.Join(tmp, ".mesh")
	waMedia := filepath.Join(meshBase, "whatsapp-accounts", "acc", "media")
	if err := os.MkdirAll(waMedia, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	writeImg := func(name string, size int) string {
		p := filepath.Join(waMedia, name)
		if err := os.WriteFile(p, bytes.Repeat([]byte("x"), size), 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		return p
	}

	t.Run("nil attachments returns nil, no error", func(t *testing.T) {
		atts, errMsg := ParseMeshSendAttachments(map[string]any{}, "larry-pa")
		if atts != nil || errMsg != "" {
			t.Errorf("got atts=%v errMsg=%q", atts, errMsg)
		}
	})

	t.Run("empty array returns nil, no error", func(t *testing.T) {
		atts, errMsg := ParseMeshSendAttachments(map[string]any{"attachments": []any{}}, "larry-pa")
		if atts != nil || errMsg != "" {
			t.Errorf("got atts=%v errMsg=%q", atts, errMsg)
		}
	})

	t.Run("two attachments → error", func(t *testing.T) {
		p := writeImg("a.jpg", 10)
		_, errMsg := ParseMeshSendAttachments(map[string]any{
			"attachments": []any{
				map[string]any{"path": p, "type": "image"},
				map[string]any{"path": p, "type": "image"},
			},
		}, "larry-pa")
		if errMsg == "" {
			t.Fatal("expected error for two attachments")
		}
		if !strings.Contains(errMsg, "only one attachment") {
			t.Errorf("unexpected error: %s", errMsg)
		}
	})

	t.Run("type audio → error", func(t *testing.T) {
		p := writeImg("a.jpg", 10)
		_, errMsg := ParseMeshSendAttachments(map[string]any{
			"attachments": []any{map[string]any{"path": p, "type": "audio"}},
		}, "larry-pa")
		if !strings.Contains(errMsg, "unsupported attachment type in v1: audio") {
			t.Errorf("unexpected error: %q", errMsg)
		}
	})

	t.Run("non-allowlisted extension → error", func(t *testing.T) {
		badPath := filepath.Join(waMedia, "bad.gif")
		_ = os.WriteFile(badPath, []byte("x"), 0o600)
		_, errMsg := ParseMeshSendAttachments(map[string]any{
			"attachments": []any{map[string]any{"path": badPath, "type": "image"}},
		}, "larry-pa")
		if !strings.Contains(errMsg, "unsupported image type") {
			t.Errorf("unexpected error: %q", errMsg)
		}
	})

	t.Run("file size over 8 MB → error", func(t *testing.T) {
		bigPath := writeImg("big.jpg", int(outboundMaxBytes)+1)
		_, errMsg := ParseMeshSendAttachments(map[string]any{
			"attachments": []any{map[string]any{"path": bigPath, "type": "image"}},
		}, "larry-pa")
		if !strings.Contains(errMsg, "attachment too large") {
			t.Errorf("unexpected error: %q", errMsg)
		}
	})

	t.Run("file does not exist → error", func(t *testing.T) {
		_, errMsg := ParseMeshSendAttachments(map[string]any{
			"attachments": []any{map[string]any{"path": filepath.Join(waMedia, "ghost.jpg"), "type": "image"}},
		}, "larry-pa")
		if !strings.Contains(errMsg, "attachment file not found") {
			t.Errorf("unexpected error: %q", errMsg)
		}
	})

	t.Run("valid image → attachment with correct MIME", func(t *testing.T) {
		p := writeImg("ok.jpg", 100)
		atts, errMsg := ParseMeshSendAttachments(map[string]any{
			"attachments": []any{map[string]any{"path": p, "type": "image"}},
		}, "larry-pa")
		if errMsg != "" {
			t.Fatalf("unexpected error: %s", errMsg)
		}
		if len(atts) != 1 {
			t.Fatalf("expected 1 attachment, got %d", len(atts))
		}
		if atts[0].MIMEType != "image/jpeg" {
			t.Errorf("MIMEType: got %q want image/jpeg", atts[0].MIMEType)
		}
		if atts[0].Path != p {
			t.Errorf("Path: got %q want %q", atts[0].Path, p)
		}
		if atts[0].Type != "image" {
			t.Errorf("Type: got %q want image", atts[0].Type)
		}
	})

	t.Run("valid .png → image/png MIME", func(t *testing.T) {
		p := writeImg("chart.png", 100)
		atts, errMsg := ParseMeshSendAttachments(map[string]any{
			"attachments": []any{map[string]any{"path": p, "type": "image"}},
		}, "larry-pa")
		if errMsg != "" || atts[0].MIMEType != "image/png" {
			t.Errorf("errMsg=%q MIMEType=%q", errMsg, atts[0].MIMEType)
		}
	})
}

// outboundMaxBytes mirrors media.OutboundMaxBytes for use in test size assertions.
const outboundMaxBytes = 8 * 1024 * 1024

// --- HandleMeshSend integration tests ---

func TestHandleMeshSendEmptyContentNoAttachment(t *testing.T) {
	var responses []JSONRPCMessage
	write := func(msg JSONRPCMessage) error {
		responses = append(responses, msg)
		return nil
	}
	var buf bytes.Buffer
	routerWr := ipc.NewWriter(&buf)

	err := HandleMeshSend(context.Background(), json.RawMessage(`1`), map[string]any{
		"target":  "peers://agent/nova",
		"content": "",
	}, routerWr, nil, nopLogf, write)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(responses) != 1 || responses[0].Error == nil {
		t.Fatalf("expected error response, got %+v", responses)
	}
	if !strings.Contains(responses[0].Error.Message, "content or attachment required") {
		t.Errorf("unexpected error message: %s", responses[0].Error.Message)
	}
}

// TestHandleMeshSendRejectsNonPeersScheme covers Plan 00-05's full-cut: the
// shim no longer accepts discord://, whatsapp://, or any other scheme — only
// peers://. (Previously this was a happy-path with discord://; now it's the
// guard.)
func TestHandleMeshSendRejectsNonPeersScheme(t *testing.T) {
	var responses []JSONRPCMessage
	write := func(msg JSONRPCMessage) error {
		responses = append(responses, msg)
		return nil
	}
	var buf bytes.Buffer
	routerWr := ipc.NewWriter(&buf)

	for _, target := range []string{"discord://channel/123", "whatsapp://chat/abc", "slack://channel/xyz"} {
		responses = responses[:0]
		err := HandleMeshSend(context.Background(), json.RawMessage(`1`), map[string]any{
			"target":  target,
			"content": "hi",
		}, routerWr, nil, nopLogf, write)
		if err != nil {
			t.Fatalf("target=%s unexpected err: %v", target, err)
		}
		if len(responses) != 1 || responses[0].Error == nil {
			t.Fatalf("target=%s: expected error response, got %+v", target, responses)
		}
		if !strings.Contains(responses[0].Error.Message, "must start with peers://") {
			t.Errorf("target=%s: unexpected error: %s", target, responses[0].Error.Message)
		}
	}
}

func TestHandleMeshSendValidImageAttachment(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	meshBase := filepath.Join(tmp, ".mesh")
	waMedia := filepath.Join(meshBase, "whatsapp-accounts", "acc", "media")
	if err := os.MkdirAll(waMedia, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	imgPath := filepath.Join(waMedia, "photo.jpg")
	if err := os.WriteFile(imgPath, bytes.Repeat([]byte("x"), 100), 0o600); err != nil {
		t.Fatalf("write img: %v", err)
	}

	var responses []JSONRPCMessage
	write := func(msg JSONRPCMessage) error {
		responses = append(responses, msg)
		return nil
	}

	var captured bytes.Buffer
	routerWr := ipc.NewWriter(&captured)

	var incOutCalled bool
	err := HandleMeshSend(context.Background(), json.RawMessage(`1`), map[string]any{
		"target":  "peers://agent/nova",
		"content": "",
		"attachments": []any{
			map[string]any{"path": imgPath, "type": "image"},
		},
	}, routerWr, func() { incOutCalled = true }, nopLogf, write)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(responses) != 1 || responses[0].Error != nil {
		t.Fatalf("expected success response, got error: %+v", responses[0].Error)
	}
	if !incOutCalled {
		t.Error("incOut was not called")
	}

	rd := ipc.NewReader(&captured)
	env, readErr := rd.Read()
	if readErr != nil {
		t.Fatalf("read envelope: %v", readErr)
	}
	if env.Type != "send" {
		t.Errorf("Type: got %q want send", env.Type)
	}
	if len(env.Attachments) != 1 {
		t.Fatalf("attachments len: got %d want 1", len(env.Attachments))
	}
	if env.Attachments[0].Path != imgPath {
		t.Errorf("Path: got %q want %q", env.Attachments[0].Path, imgPath)
	}
	if env.Attachments[0].MIMEType != "image/jpeg" {
		t.Errorf("MIMEType: got %q want image/jpeg", env.Attachments[0].MIMEType)
	}
}

func TestMeshLastInboundReplyTarget(t *testing.T) {
	l := &MeshLastInbound{}

	source, target := l.ReplyTarget()
	if source != "" || target != "" {
		t.Errorf("expected empty source+target before any inbound, got source=%q target=%q", source, target)
	}

	// Plan 00-05 collapsed ReplyTarget to peers-only; discord/whatsapp
	// sources can still be recorded for telemetry but no longer produce a
	// constructible reply URL.
	l.Set("peers", "nova")
	source, target = l.ReplyTarget()
	if source != "peers" {
		t.Errorf("source: got %q want peers", source)
	}
	if target != "peers://agent/nova" {
		t.Errorf("target: got %q want peers://agent/nova", target)
	}

	// A discord-source inbound returns the source (telemetry preserved) but
	// an empty target string so HandleMeshReply surfaces the "cannot
	// construct reply target for source discord" error.
	l.Set("discord", "123456789")
	source, target = l.ReplyTarget()
	if source != "discord" {
		t.Errorf("source: got %q want discord", source)
	}
	if target != "" {
		t.Errorf("target: got %q want empty (Plan 00-05 dropped discord:// scheme)", target)
	}
}
