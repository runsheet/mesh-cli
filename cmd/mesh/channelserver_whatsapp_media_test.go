package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// --- pruneWhatsappMedia tests ---

func writeMediaFile(t *testing.T, dir, name string, size int, mtime time.Time) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), size), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
	return path
}

func TestPruneWhatsappMediaTTL(t *testing.T) {
	dir := t.TempDir()

	now := time.Now()
	old := now.Add(-(mediaRetentionDays + 1) * 24 * time.Hour)
	fresh := now.Add(-1 * time.Hour)

	oldPath := writeMediaFile(t, dir, "old.jpg", 100, old)
	freshPath := writeMediaFile(t, dir, "fresh.jpg", 100, fresh)

	// pruneWhatsappMedia works on the account media dir; patch it by creating
	// the structure it expects via a fake account name that maps to dir.
	// We can't easily inject the dir, so call the prune logic directly on
	// the temp dir using the internal helper.
	pruneMediaDir(dir)

	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Errorf("old file should have been pruned, err=%v", err)
	}
	if _, err := os.Stat(freshPath); err != nil {
		t.Errorf("fresh file should survive, err=%v", err)
	}
}

func TestPruneWhatsappMediaByteCap(t *testing.T) {
	dir := t.TempDir()

	now := time.Now()
	// All files are fresh (within TTL). Sizes chosen to exceed cap when combined.
	// cap = mediaMaxBytes = 200 MB; we fake a tiny cap by overriding via pruneMediaDirWithLimits.
	// We test the ordering (oldest-first) and the cap logic directly.
	t0 := now.Add(-3 * time.Hour)
	t1 := now.Add(-2 * time.Hour)
	t2 := now.Add(-1 * time.Hour)

	oldest := writeMediaFile(t, dir, "a.jpg", 60, t0)
	middle := writeMediaFile(t, dir, "b.jpg", 60, t1)
	newest := writeMediaFile(t, dir, "c.jpg", 60, t2)

	// Cap = 100 bytes → oldest two files (120 bytes) must be pruned until ≤ 100.
	// After dropping "a.jpg" (60 bytes): total = 120 → still over.
	// After dropping "b.jpg" (60 bytes): total = 60 → under cap.
	pruneMediaDirWithLimits(dir, 365*24*time.Hour, 100)

	if _, err := os.Stat(oldest); !os.IsNotExist(err) {
		t.Errorf("oldest file should be pruned")
	}
	if _, err := os.Stat(middle); !os.IsNotExist(err) {
		t.Errorf("middle file should be pruned to get under cap")
	}
	if _, err := os.Stat(newest); err != nil {
		t.Errorf("newest file should survive, err=%v", err)
	}
}

func TestPruneWhatsappMediaComposed(t *testing.T) {
	dir := t.TempDir()

	now := time.Now()
	oldTime := now.Add(-(mediaRetentionDays + 1) * 24 * time.Hour)
	freshTime := now.Add(-1 * time.Hour)

	old1 := writeMediaFile(t, dir, "old1.jpg", 40, oldTime)
	old2 := writeMediaFile(t, dir, "old2.jpg", 40, oldTime)
	fresh1 := writeMediaFile(t, dir, "fresh1.jpg", 40, freshTime)
	fresh2 := writeMediaFile(t, dir, "fresh2.jpg", 40, freshTime)

	// TTL prune removes old1+old2 (80 bytes). Remaining: fresh1+fresh2 = 80 bytes.
	// Byte cap = 50 → oldest fresh file (fresh1) must also be pruned.
	pruneMediaDirWithLimits(dir, mediaRetentionDays*24*time.Hour, 50)

	for _, p := range []string{old1, old2, fresh1} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("expected %s to be pruned", filepath.Base(p))
		}
	}
	if _, err := os.Stat(fresh2); err != nil {
		t.Errorf("fresh2 should survive, err=%v", err)
	}
}

// --- ipcAttachment round-trip tests ---

func TestIPCAttachmentRoundTrip(t *testing.T) {
	env := ipcEnvelope{
		V:          ipcVersion,
		Type:       "inbound",
		RoutingKey: "123@g.us",
		Content:    "hello",
		Attachments: []ipcAttachment{
			{Type: "image", Path: "/abs/path/file.jpg", MIMEType: "image/jpeg"},
		},
	}

	var buf bytes.Buffer
	wr := newIPCWriter(&buf)
	if err := wr.Write(env); err != nil {
		t.Fatalf("write: %v", err)
	}

	rd := newIPCReader(&buf)
	got, err := rd.Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got.Attachments) != 1 {
		t.Fatalf("attachments len: got %d want 1", len(got.Attachments))
	}
	att := got.Attachments[0]
	if att.Type != "image" {
		t.Errorf("type: got %q want image", att.Type)
	}
	if att.Path != "/abs/path/file.jpg" {
		t.Errorf("path: got %q", att.Path)
	}
	if att.MIMEType != "image/jpeg" {
		t.Errorf("mimeType: got %q", att.MIMEType)
	}
	if att.FailReason != "" {
		t.Errorf("failReason: got %q want empty", att.FailReason)
	}
}

func TestIPCAttachmentFailureRoundTrip(t *testing.T) {
	env := ipcEnvelope{
		V:    ipcVersion,
		Type: "inbound",
		Attachments: []ipcAttachment{
			{Type: "image", MIMEType: "image/jpeg", FailReason: "download timeout after 30s"},
		},
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
		t.Fatalf("attachments len: got %d", len(got.Attachments))
	}
	if got.Attachments[0].FailReason != "download timeout after 30s" {
		t.Errorf("failReason: got %q", got.Attachments[0].FailReason)
	}
	if got.Attachments[0].Path != "" {
		t.Errorf("path should be empty on failure, got %q", got.Attachments[0].Path)
	}
}

func TestIPCEnvelopeWithoutAttachmentsOmitsField(t *testing.T) {
	env := ipcEnvelope{V: ipcVersion, Type: "inbound", Content: "text only"}
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(data, []byte("attachments")) {
		t.Errorf("text-only envelope should not include attachments field: %s", data)
	}

	var got ipcEnvelope
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Attachments != nil {
		t.Errorf("attachments should be nil for text-only envelope")
	}
}

// --- formatInboundContent tests ---

func TestFormatInboundContent(t *testing.T) {
	cases := []struct {
		name        string
		caption     string
		attachments []ipcAttachment
		want        string
	}{
		{
			name:    "text only, no attachment",
			caption: "hello",
			want:    "hello",
		},
		{
			name:    "no caption, no attachment",
			caption: "",
			want:    "",
		},
		{
			name:    "image happy path, no caption",
			caption: "",
			attachments: []ipcAttachment{
				{Type: "image", Path: "/abs/path/file.jpg", MIMEType: "image/jpeg"},
			},
			want: "[image: /abs/path/file.jpg]",
		},
		{
			name:    "image happy path, with caption",
			caption: "test image",
			attachments: []ipcAttachment{
				{Type: "image", Path: "/abs/path/file.jpg", MIMEType: "image/jpeg"},
			},
			want: "test image\n[image: /abs/path/file.jpg]",
		},
		{
			name:    "image download failed, no caption",
			caption: "",
			attachments: []ipcAttachment{
				{Type: "image", MIMEType: "image/jpeg", FailReason: "download timeout after 30s"},
			},
			want: "[image: download_failed reason=download timeout after 30s]",
		},
		{
			name:    "image download failed, with caption",
			caption: "look at this",
			attachments: []ipcAttachment{
				{Type: "image", MIMEType: "image/jpeg", FailReason: "download timeout after 30s"},
			},
			want: "look at this\n[image: download_failed reason=download timeout after 30s]",
		},
		{
			name:    "unsupported media type (OQ-3: image label preserved when Type==image)",
			caption: "",
			attachments: []ipcAttachment{
				{Type: "image", MIMEType: "audio/ogg", FailReason: "unsupported media type audio/ogg"},
			},
			want: "[image: download_failed reason=unsupported media type audio/ogg]",
		},
		{
			name:    "empty attachment (no path, no reason) is skipped",
			caption: "hi",
			attachments: []ipcAttachment{
				{Type: "image"},
			},
			want: "hi",
		},
		// --- spec 16 (audio) cases ---
		{
			name:    "audio happy path, no caption",
			caption: "",
			attachments: []ipcAttachment{
				{Type: "audio", Path: "/abs/path/file.ogg", MIMEType: "audio/ogg"},
			},
			want: "[audio: /abs/path/file.ogg]",
		},
		{
			name:    "audio happy path, with caption",
			caption: "voice note",
			attachments: []ipcAttachment{
				{Type: "audio", Path: "/abs/path/file.ogg", MIMEType: "audio/ogg; codecs=opus"},
			},
			want: "voice note\n[audio: /abs/path/file.ogg]",
		},
		{
			name:    "audio download failed, no caption",
			caption: "",
			attachments: []ipcAttachment{
				{Type: "audio", MIMEType: "audio/ogg", FailReason: "download timeout after 30s"},
			},
			want: "[audio: download_failed reason=download timeout after 30s]",
		},
		{
			name:    "audio download failed, with caption",
			caption: "sent you a voice note",
			attachments: []ipcAttachment{
				{Type: "audio", MIMEType: "audio/mpeg", FailReason: "download timeout after 30s"},
			},
			want: "sent you a voice note\n[audio: download_failed reason=download timeout after 30s]",
		},
		{
			name:    "audio unsupported mime gets audio label on failure",
			caption: "",
			attachments: []ipcAttachment{
				{Type: "audio", MIMEType: "audio/flac", FailReason: "unsupported media type audio/flac"},
			},
			want: "[audio: download_failed reason=unsupported media type audio/flac]",
		},
		{
			name:    "empty type defaults to image label (back-compat)",
			caption: "",
			attachments: []ipcAttachment{
				{Type: "", Path: "/abs/path/file.jpg", MIMEType: "image/jpeg"},
			},
			want: "[image: /abs/path/file.jpg]",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatInboundContent(tc.caption, tc.attachments)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMimeToExtAudio(t *testing.T) {
	// Verify spec 16 (audio) MIME entries. Base types only — callers strip
	// ";codecs=…" parameters before lookup.
	cases := []struct {
		mime    string
		wantExt string
	}{
		{"audio/ogg", ".ogg"},
		{"audio/mpeg", ".mp3"},
		{"audio/mp4", ".m4a"},
		{"audio/aac", ".aac"},
	}
	for _, tc := range cases {
		ext, ok := mimeToExt[tc.mime]
		if !ok {
			t.Errorf("mimeToExt[%q] missing", tc.mime)
			continue
		}
		if ext != tc.wantExt {
			t.Errorf("mimeToExt[%q] = %q, want %q", tc.mime, ext, tc.wantExt)
		}
	}
	// Confirm image entries are still present.
	for _, mime := range []string{"image/jpeg", "image/png", "image/webp"} {
		if _, ok := mimeToExt[mime]; !ok {
			t.Errorf("mimeToExt[%q] missing (image regression)", mime)
		}
	}
}
