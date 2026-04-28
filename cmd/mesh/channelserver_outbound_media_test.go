package main

import (
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

// TestParseMeshSendAttachments moved to internal/shim/shim_test.go (spec22-phase6).

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

// TestHandleMeshSend* and TestParseMeshSendAttachments moved to
// internal/shim/shim_test.go (spec22-phase6).
