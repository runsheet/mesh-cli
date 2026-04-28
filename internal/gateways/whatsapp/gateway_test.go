package whatsapp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// mockDownloadableMessage implements whatsmeow.DownloadableMessage with no-op
// field accessors. The actual bytes are returned by the injected downloadFn —
// the message struct itself is never inspected by downloadMedia.
type mockDownloadableMessage struct{}

func (mockDownloadableMessage) GetDirectPath() string   { return "" }
func (mockDownloadableMessage) GetMediaKey() []byte     { return nil }
func (mockDownloadableMessage) GetFileSHA256() []byte   { return nil }
func (mockDownloadableMessage) GetFileEncSHA256() []byte { return nil }

// compile-time check
var _ whatsmeow.DownloadableMessage = mockDownloadableMessage{}

func TestDownloadWhatsappMedia(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	canned := []byte("fake-image-bytes")
	account := "testaccount"

	gw := newGateway(account)
	gw.downloadFn = func(_ context.Context, _ *whatsmeow.Client, _ whatsmeow.DownloadableMessage) ([]byte, error) {
		return canned, nil
	}

	evt := &events.Message{
		Info: types.MessageInfo{ID: "MSGID001"},
	}

	t.Run("nil client returns FailReason", func(t *testing.T) {
		att := gw.downloadMedia(evt, mockDownloadableMessage{}, "image", "image/jpeg", nil, nopLogf)
		if att.FailReason == "" {
			t.Fatal("expected FailReason for nil client, got empty")
		}
		if att.Path != "" {
			t.Errorf("expected empty Path for nil-client failure, got %q", att.Path)
		}
		if att.MIMEType != "image/jpeg" {
			t.Errorf("MIMEType: got %q want image/jpeg", att.MIMEType)
		}
	})

	t.Run("unsupported mime returns FailReason", func(t *testing.T) {
		att := gw.downloadMedia(evt, mockDownloadableMessage{}, "image", "video/mp4", nil, nopLogf)
		if att.FailReason == "" {
			t.Fatal("expected FailReason for unsupported MIME, got empty")
		}
		if !strings.Contains(att.FailReason, "unsupported media type") {
			t.Errorf("FailReason %q does not mention unsupported media type", att.FailReason)
		}
	})

	t.Run("download ok writes file and returns Path and MIMEType", func(t *testing.T) {
		mediaDir := filepath.Join(tmp, ".mesh", "whatsapp-accounts", account, "media")
		expectedPath := filepath.Join(mediaDir, "MSGID001.jpg")

		fakeClient := &whatsmeow.Client{}
		att := gw.downloadMedia(evt, mockDownloadableMessage{}, "image", "image/jpeg", fakeClient, nopLogf)

		if att.FailReason != "" {
			t.Fatalf("unexpected FailReason: %s", att.FailReason)
		}
		if att.Path != expectedPath {
			t.Errorf("Path: got %q want %q", att.Path, expectedPath)
		}
		if att.MIMEType != "image/jpeg" {
			t.Errorf("MIMEType: got %q want image/jpeg", att.MIMEType)
		}
		if att.Type != "image" {
			t.Errorf("Type: got %q want image", att.Type)
		}

		got, err := os.ReadFile(att.Path)
		if err != nil {
			t.Fatalf("ReadFile %s: %v", att.Path, err)
		}
		if string(got) != string(canned) {
			t.Errorf("file contents: got %q want %q", got, canned)
		}
	})

	t.Run("audio mime strips codecs suffix via downloadAudio", func(t *testing.T) {
		fakeClient := &whatsmeow.Client{}
		audEvt := &events.Message{Info: types.MessageInfo{ID: "AUDID001"}}

		mediaDir := filepath.Join(tmp, ".mesh", "whatsapp-accounts", account, "media")
		expectedPath := filepath.Join(mediaDir, "AUDID001.ogg")

		rawMime := "audio/ogg; codecs=opus"
		att := gw.downloadMedia(audEvt, mockDownloadableMessage{}, "audio", "audio/ogg", fakeClient, nopLogf)

		if att.Path != expectedPath {
			t.Errorf("audio path: got %q want %q", att.Path, expectedPath)
		}
		if att.MIMEType != "audio/ogg" {
			t.Errorf("audio MIMEType: got %q want audio/ogg", att.MIMEType)
		}
		_ = rawMime
	})
}

func nopLogf(_ string, _ ...any) {}
