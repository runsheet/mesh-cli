package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- validateAttachmentPath tests ---

func TestValidateAttachmentPath(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	meshBase := filepath.Join(tmp, ".mesh")

	// Create the allowed directories.
	waMedia := filepath.Join(meshBase, "whatsapp-accounts", "myaccount", "media")
	dcMedia := filepath.Join(meshBase, "discord-accounts", "myaccount", "media")
	agentDir := filepath.Join(meshBase, "agents", "larry-pa")
	for _, d := range []string{waMedia, dcMedia, agentDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	writeFile := func(dir, name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		return p
	}

	t.Run("allowed: whatsapp media dir", func(t *testing.T) {
		p := writeFile(waMedia, "img.jpg")
		if err := validateAttachmentPath(p, "larry-pa"); err != nil {
			t.Errorf("expected nil, got %v", err)
		}
	})

	t.Run("allowed: discord media dir", func(t *testing.T) {
		p := writeFile(dcMedia, "img.png")
		if err := validateAttachmentPath(p, "larry-pa"); err != nil {
			t.Errorf("expected nil, got %v", err)
		}
	})

	t.Run("allowed: agent workdir", func(t *testing.T) {
		p := writeFile(agentDir, "chart.png")
		if err := validateAttachmentPath(p, "larry-pa"); err != nil {
			t.Errorf("expected nil, got %v", err)
		}
	})

	t.Run("allowed: symlink pointing inside allowed root", func(t *testing.T) {
		target := writeFile(waMedia, "real.jpg")
		link := filepath.Join(agentDir, "link.jpg")
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlink creation failed (likely permissions): %v", err)
		}
		if err := validateAttachmentPath(link, "larry-pa"); err != nil {
			t.Errorf("symlink inside allowed root should be allowed, got %v", err)
		}
	})

	t.Run("reject: path outside all roots", func(t *testing.T) {
		// Write a real file in /tmp to ensure Stat passes but prefix check fails.
		outside := writeFile(t.TempDir(), "foo.jpg")
		err := validateAttachmentPath(outside, "larry-pa")
		if err == nil {
			t.Fatal("expected error for path outside allowed roots, got nil")
		}
		if !strings.Contains(err.Error(), "attachment path not in an allowed directory") {
			t.Errorf("unexpected error message: %v", err)
		}
	})

	t.Run("reject: symlink escaping to outside allowed root", func(t *testing.T) {
		outsideTarget := writeFile(t.TempDir(), "secret.txt")
		link := filepath.Join(agentDir, "escape.jpg")
		if err := os.Symlink(outsideTarget, link); err != nil {
			t.Skipf("symlink creation failed: %v", err)
		}
		err := validateAttachmentPath(link, "larry-pa")
		if err == nil {
			t.Fatal("expected error for symlink escaping allowed root, got nil")
		}
	})

	t.Run("reject: path traversal attempt", func(t *testing.T) {
		// Construct a traversal path — EvalSymlinks will fail or resolve to outside.
		traversal := filepath.Join(agentDir, "..", "..", "node-token")
		err := validateAttachmentPath(traversal, "larry-pa")
		// Either EvalSymlinks fails (file doesn't exist → error) or the resolved
		// path is outside allowed roots.
		if err == nil {
			t.Fatal("expected error for path traversal attempt, got nil")
		}
	})

	t.Run("reject: non-existent path returns error", func(t *testing.T) {
		err := validateAttachmentPath(filepath.Join(waMedia, "nonexistent.jpg"), "larry-pa")
		if err == nil {
			t.Fatal("expected error for non-existent path")
		}
	})
}

// --- parseMeshSendAttachments validation tests ---

func TestParseMeshSendAttachments(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

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
		atts, errMsg := parseMeshSendAttachments(map[string]any{}, "larry-pa")
		if atts != nil || errMsg != "" {
			t.Errorf("got atts=%v errMsg=%q", atts, errMsg)
		}
	})

	t.Run("empty array returns nil, no error", func(t *testing.T) {
		atts, errMsg := parseMeshSendAttachments(map[string]any{"attachments": []any{}}, "larry-pa")
		if atts != nil || errMsg != "" {
			t.Errorf("got atts=%v errMsg=%q", atts, errMsg)
		}
	})

	t.Run("two attachments → error", func(t *testing.T) {
		p := writeImg("a.jpg", 10)
		_, errMsg := parseMeshSendAttachments(map[string]any{
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
		_, errMsg := parseMeshSendAttachments(map[string]any{
			"attachments": []any{map[string]any{"path": p, "type": "audio"}},
		}, "larry-pa")
		if !strings.Contains(errMsg, "unsupported attachment type in v1: audio") {
			t.Errorf("unexpected error: %q", errMsg)
		}
	})

	t.Run("non-allowlisted extension → error", func(t *testing.T) {
		p := writeImg("video.mp4", 10)
		// Rename won't work since writeImg uses the name directly.
		// Write a fresh file with a disallowed extension.
		badPath := filepath.Join(waMedia, "bad.gif")
		_ = os.WriteFile(badPath, []byte("x"), 0o600)
		_, errMsg := parseMeshSendAttachments(map[string]any{
			"attachments": []any{map[string]any{"path": badPath, "type": "image"}},
		}, "larry-pa")
		_ = p
		if !strings.Contains(errMsg, "unsupported image type") {
			t.Errorf("unexpected error: %q", errMsg)
		}
	})

	t.Run("file size over 8 MB → error", func(t *testing.T) {
		bigPath := writeImg("big.jpg", outboundMaxBytes+1)
		_, errMsg := parseMeshSendAttachments(map[string]any{
			"attachments": []any{map[string]any{"path": bigPath, "type": "image"}},
		}, "larry-pa")
		if !strings.Contains(errMsg, "attachment too large") {
			t.Errorf("unexpected error: %q", errMsg)
		}
	})

	t.Run("file does not exist → error", func(t *testing.T) {
		_, errMsg := parseMeshSendAttachments(map[string]any{
			"attachments": []any{map[string]any{"path": filepath.Join(waMedia, "ghost.jpg"), "type": "image"}},
		}, "larry-pa")
		if !strings.Contains(errMsg, "attachment file not found") {
			t.Errorf("unexpected error: %q", errMsg)
		}
	})

	t.Run("valid image → attachment with correct MIME", func(t *testing.T) {
		p := writeImg("ok.jpg", 100)
		atts, errMsg := parseMeshSendAttachments(map[string]any{
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
		atts, errMsg := parseMeshSendAttachments(map[string]any{
			"attachments": []any{map[string]any{"path": p, "type": "image"}},
		}, "larry-pa")
		if errMsg != "" || atts[0].MIMEType != "image/png" {
			t.Errorf("errMsg=%q MIMEType=%q", errMsg, atts[0].MIMEType)
		}
	})
}

// --- ipcSendEnvWithAttachments round-trip tests ---

func TestIPCSendEnvWithAttachmentsRoundTrip(t *testing.T) {
	atts := []ipcAttachment{
		{Type: "image", Path: "/abs/path/img.jpg", MIMEType: "image/jpeg"},
	}
	env := ipcSendEnvWithAttachments("whatsapp://chat/123@g.us", "123@g.us", "hello", atts)

	if env.Type != "send" {
		t.Errorf("Type: got %q want send", env.Type)
	}
	if env.Target != "whatsapp://chat/123@g.us" {
		t.Errorf("Target: got %q", env.Target)
	}
	if env.Content != "hello" {
		t.Errorf("Content: got %q", env.Content)
	}

	data, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got ipcEnvelope
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Attachments) != 1 {
		t.Fatalf("attachments len: got %d want 1", len(got.Attachments))
	}
	if got.Attachments[0].Path != "/abs/path/img.jpg" {
		t.Errorf("Path: got %q", got.Attachments[0].Path)
	}
	if got.Attachments[0].MIMEType != "image/jpeg" {
		t.Errorf("MIMEType: got %q", got.Attachments[0].MIMEType)
	}
}

func TestIPCSendEnvWithNilAttachmentsWireIdentical(t *testing.T) {
	// A nil-attachments ipcSendEnvWithAttachments envelope must produce the
	// same wire bytes as ipcSendEnv (omitempty keeps the field absent).
	plain := ipcSendEnv("chan123", "hello world")
	withNil := ipcSendEnvWithAttachments("", "chan123", "hello world", nil)

	// Both should have no attachments field in JSON.
	plainJSON, _ := json.Marshal(plain)
	withNilJSON, _ := json.Marshal(withNil)

	var plainMap, withNilMap map[string]any
	_ = json.Unmarshal(plainJSON, &plainMap)
	_ = json.Unmarshal(withNilJSON, &withNilMap)

	if _, has := plainMap["attachments"]; has {
		t.Error("ipcSendEnv should not include attachments field")
	}
	if _, has := withNilMap["attachments"]; has {
		t.Error("ipcSendEnvWithAttachments(nil) should not include attachments field")
	}
	if plainMap["channelId"] != withNilMap["channelId"] {
		t.Errorf("channelId mismatch: %v vs %v", plainMap["channelId"], withNilMap["channelId"])
	}
	if plainMap["content"] != withNilMap["content"] {
		t.Errorf("content mismatch: %v vs %v", plainMap["content"], withNilMap["content"])
	}
}

// --- handleMeshSend integration tests (mock ipcWriter) ---

type captureWriter struct {
	buf bytes.Buffer
}

func (c *captureWriter) Write(env ipcEnvelope) error {
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	c.buf.Write(data)
	c.buf.WriteByte('\n')
	return nil
}

func TestHandleMeshSendEmptyContentNoAttachment(t *testing.T) {
	var responses []jsonrpcMessage
	write := func(msg jsonrpcMessage) error {
		responses = append(responses, msg)
		return nil
	}
	cw := &captureWriter{}
	routerWr := newIPCWriter(&cw.buf)

	err := handleMeshSend(nil, json.RawMessage(`1`), map[string]any{
		"target":  "discord://channel/123",
		"content": "",
	}, routerWr, &meshShimMetrics{}, nopLogf, write)

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

	var responses []jsonrpcMessage
	write := func(msg jsonrpcMessage) error {
		responses = append(responses, msg)
		return nil
	}

	var captured bytes.Buffer
	routerWr := newIPCWriter(&captured)

	err := handleMeshSend(nil, json.RawMessage(`1`), map[string]any{
		"target":  "whatsapp://chat/123@g.us",
		"content": "",
		"attachments": []any{
			map[string]any{"path": imgPath, "type": "image"},
		},
	}, routerWr, &meshShimMetrics{}, nopLogf, write)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(responses) != 1 || responses[0].Error != nil {
		t.Fatalf("expected success response, got error: %+v", responses[0].Error)
	}

	// Verify the IPC envelope was written with attachments.
	rd := newIPCReader(&captured)
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
