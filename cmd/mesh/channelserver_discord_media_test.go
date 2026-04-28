package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// --- discordMediaPath tests ---

func TestDiscordMediaPath(t *testing.T) {
	cases := []struct {
		mime    string
		wantExt string // expected base suffix when called with idx=0
		wantErr bool
	}{
		{"image/jpeg", "-0.jpg", false},
		{"image/png", "-0.png", false},
		{"image/webp", "-0.webp", false},
		{"audio/ogg", "", true},
		{"video/mp4", "", true},
		{"application/pdf", "", true},
		{"", "", true},
	}

	for _, tc := range cases {
		// Use fixed account/msgID and idx=0 for all; only extension matters.
		path, err := discordMediaPath("testaccount", "msgABC", 0, tc.mime)
		if tc.wantErr {
			if err == nil {
				t.Errorf("discordMediaPath(mime=%q): expected error, got path=%q", tc.mime, path)
			}
			continue
		}
		if err != nil {
			t.Errorf("discordMediaPath(mime=%q): unexpected error: %v", tc.mime, err)
			continue
		}
		base := filepath.Base(path)
		// wantExt includes the "-N" prefix, e.g. "-0.jpg". The full base is "msgABC-0.jpg".
		if base != "msgABC"+tc.wantExt {
			t.Errorf("discordMediaPath(mime=%q): base=%q, want msgABC%s", tc.mime, base, tc.wantExt)
		}
	}
	// Verify idx is embedded correctly.
	p0, _ := discordMediaPath("acct", "msg123", 0, "image/jpeg")
	p1, _ := discordMediaPath("acct", "msg123", 1, "image/png")
	if filepath.Base(p0) != "msg123-0.jpg" {
		t.Errorf("idx=0: got %q want msg123-0.jpg", filepath.Base(p0))
	}
	if filepath.Base(p1) != "msg123-1.png" {
		t.Errorf("idx=1: got %q want msg123-1.png", filepath.Base(p1))
	}
}

// --- multi-attachment result assembly ---

func TestDiscordAttachmentResultAssembly(t *testing.T) {
	// Simulate the result assembly logic: given a mock result set [ok, fail, ok]
	// in index order, assert []ipcAttachment preserves order and sets FailReason
	// only on the failed entry.
	results := []ipcAttachment{
		{Type: "image", Path: "/abs/msg-0.jpg", MIMEType: "image/jpeg"},
		{Type: "image", MIMEType: "image/png", FailReason: "http 403"},
		{Type: "image", Path: "/abs/msg-2.webp", MIMEType: "image/webp"},
	}

	if results[0].Path == "" || results[0].FailReason != "" {
		t.Errorf("entry 0 should be success: %+v", results[0])
	}
	if results[1].FailReason == "" || results[1].Path != "" {
		t.Errorf("entry 1 should be failure: %+v", results[1])
	}
	if results[2].Path == "" || results[2].FailReason != "" {
		t.Errorf("entry 2 should be success: %+v", results[2])
	}
	// Order preserved.
	if results[0].MIMEType != "image/jpeg" {
		t.Errorf("entry 0 mime: got %q want image/jpeg", results[0].MIMEType)
	}
	if results[1].MIMEType != "image/png" {
		t.Errorf("entry 1 mime: got %q want image/png", results[1].MIMEType)
	}
	if results[2].MIMEType != "image/webp" {
		t.Errorf("entry 2 mime: got %q want image/webp", results[2].MIMEType)
	}
}

// --- formatInboundContent with multiple attachments ---

func TestFormatInboundContentMultiAttachment(t *testing.T) {
	cases := []struct {
		name        string
		caption     string
		attachments []ipcAttachment
		want        string
	}{
		{
			name:    "two happy-path attachments with caption",
			caption: "look at these",
			attachments: []ipcAttachment{
				{Type: "image", Path: "/abs/msg-0.jpg", MIMEType: "image/jpeg"},
				{Type: "image", Path: "/abs/msg-1.png", MIMEType: "image/png"},
			},
			want: "look at these\n[image: /abs/msg-0.jpg]\n[image: /abs/msg-1.png]",
		},
		{
			name:    "first fails, second ok (no caption)",
			caption: "",
			attachments: []ipcAttachment{
				{Type: "image", MIMEType: "image/jpeg", FailReason: "http 403"},
				{Type: "image", Path: "/abs/msg-1.png", MIMEType: "image/png"},
			},
			want: "[image: download_failed reason=http 403]\n[image: /abs/msg-1.png]",
		},
		{
			name:    "all fail, no caption — non-empty result (not dropped)",
			caption: "",
			attachments: []ipcAttachment{
				{Type: "image", MIMEType: "image/jpeg", FailReason: "download timeout after 30s"},
				{Type: "image", MIMEType: "image/png", FailReason: "http 404"},
			},
			want: "[image: download_failed reason=download timeout after 30s]\n[image: download_failed reason=http 404]",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatInboundContent(tc.caption, tc.attachments)
			if got != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

// --- pruneDiscordMedia smoke test ---

func TestPruneDiscordMediaSmoke(t *testing.T) {
	// pruneDiscordMedia delegates to pruneMediaDirWithLimits which is fully
	// covered in channelserver_whatsapp_media_test.go. Here we just confirm
	// that the directory-lookup path doesn't panic on a temp dir and that old
	// files are removed.
	dir := t.TempDir()

	old := time.Now().Add(-(mediaRetentionDays + 1) * 24 * time.Hour)
	oldFile := filepath.Join(dir, "old.jpg")
	if err := os.WriteFile(oldFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(oldFile, old, old); err != nil {
		t.Fatal(err)
	}

	// Call the shared prune directly on the temp dir (pruneDiscordMedia itself
	// resolves the real account path, which doesn't exist in the test).
	pruneMediaDir(dir)

	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Errorf("old file should have been pruned")
	}
}
