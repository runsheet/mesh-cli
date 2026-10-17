package triplinetest

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// envelopeURLs reads a DSN the way a Sentry client does and returns the
// ingest's envelope and store URLs plus the key the DSN carries.
func envelopeURLs(t *testing.T, dsn string) (envelope, store, key string) {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DSN %q: %v", dsn, err)
	}
	project := strings.TrimPrefix(u.Path, "/")
	base := fmt.Sprintf("%s://%s", u.Scheme, u.Host)
	return base + "/api/" + project + "/envelope/",
		base + "/api/" + project + "/store/",
		u.User.Username()
}

func post(t *testing.T, target string, body []byte, gz bool, key string) *http.Response {
	t.Helper()
	payload := body
	if gz {
		var buf bytes.Buffer
		w := gzip.NewWriter(&buf)
		if _, err := w.Write(body); err != nil {
			t.Fatalf("gzip write: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("gzip close: %v", err)
		}
		payload = buf.Bytes()
	}

	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if gz {
		req.Header.Set("Content-Encoding", "gzip")
	}
	req.Header.Set("Content-Type", "application/x-sentry-envelope")
	req.Header.Set("X-Sentry-Auth", fmt.Sprintf("Sentry sentry_version=7, sentry_client=triplinetest/1.0, sentry_key=%s", key))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", target, err)
	}
	return resp
}

func eventEnvelope(eventID, payload string) []byte {
	header := fmt.Sprintf(`{"event_id":%q}`, eventID)
	itemHeader := fmt.Sprintf(`{"type":"event","length":%d}`, len(payload))
	return []byte(header + "\n" + itemHeader + "\n" + payload + "\n")
}

func TestPlainEnvelopeRecordsTheEvent(t *testing.T) {
	s := NewServer(t)
	envelope, _, key := envelopeURLs(t, s.DSN())

	payload := `{"event_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","message":"boom","tags":{"item":"3/4"}}`
	resp := post(t, envelope, eventEnvelope("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", payload), false, key)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	events := s.Events()
	if len(events) != 1 {
		t.Fatalf("Events() = %d, want 1", len(events))
	}
	if events[0].Message != "boom" {
		t.Errorf("Message = %q, want boom", events[0].Message)
	}
	if events[0].Tags["item"] != "3/4" {
		t.Errorf("Tags[item] = %q, want 3/4", events[0].Tags["item"])
	}
}

func TestGzipEnvelopeRecordsTheEvent(t *testing.T) {
	s := NewServer(t)
	envelope, _, key := envelopeURLs(t, s.DSN())

	payload := `{"event_id":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","message":"gzipped boom"}`
	resp := post(t, envelope, eventEnvelope("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", payload), true, key)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	events := s.Events()
	if len(events) != 1 {
		t.Fatalf("Events() = %d, want 1", len(events))
	}
	if events[0].Message != "gzipped boom" {
		t.Errorf("Message = %q, want %q", events[0].Message, "gzipped boom")
	}
}

func TestMultiItemEnvelopeIgnoresTheNonEventItem(t *testing.T) {
	s := NewServer(t)
	envelope, _, key := envelopeURLs(t, s.DSN())

	eventPayload := `{"event_id":"cccccccccccccccccccccccccccccccc","message":"real event"}`
	attachmentPayload := "not an event, just bytes"

	body := strings.Join([]string{
		`{"event_id":"cccccccccccccccccccccccccccccccc"}`,
		fmt.Sprintf(`{"type":"attachment","length":%d,"filename":"log.txt"}`, len(attachmentPayload)),
		attachmentPayload,
		fmt.Sprintf(`{"type":"event","length":%d}`, len(eventPayload)),
		eventPayload,
		"",
	}, "\n")

	resp := post(t, envelope, []byte(body), false, key)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	events := s.Events()
	if len(events) != 1 {
		t.Fatalf("Events() = %d, want 1 (the attachment item should be ignored)", len(events))
	}
	if events[0].Message != "real event" {
		t.Errorf("Message = %q, want %q", events[0].Message, "real event")
	}
}

func TestWrongAuthKeyIsRefused(t *testing.T) {
	s := NewServer(t)
	envelope, _, _ := envelopeURLs(t, s.DSN())

	payload := `{"event_id":"dddddddddddddddddddddddddddddddd","message":"should not land"}`
	resp := post(t, envelope, eventEnvelope("dddddddddddddddddddddddddddddddd", payload), false, "wrong-key")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}

	if events := s.Events(); len(events) != 0 {
		t.Fatalf("Events() = %d, want 0 after a refused auth", len(events))
	}
}

func TestEventsSharingAFingerprintReadBackAsOneIssue(t *testing.T) {
	s := NewServer(t)
	envelope, _, key := envelopeURLs(t, s.DSN())

	for i := 0; i < 3; i++ {
		eventID := fmt.Sprintf("%032d", i)
		payload := fmt.Sprintf(
			`{"event_id":%q,"exception":{"values":[{"type":"ClaimLost","value":"session stopped"}]},"fingerprint":["claim-lost"],"tags":{"node":"n1"}}`,
			eventID,
		)
		resp := post(t, envelope, eventEnvelope(eventID, payload), false, key)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
	}

	if !s.Wait(3, time.Second) {
		t.Fatalf("Wait(3, 1s) timed out; got %d events", len(s.Events()))
	}

	issues := s.Issues()
	if len(issues) != 1 {
		t.Fatalf("Issues() = %d, want 1", len(issues))
	}
	if issues[0].Count != 3 {
		t.Errorf("Count = %d, want 3", issues[0].Count)
	}
	if issues[0].Fingerprint != "claim-lost" {
		t.Errorf("Fingerprint = %q, want claim-lost", issues[0].Fingerprint)
	}
	if issues[0].Tags["node"] != "n1" {
		t.Errorf("Tags[node] = %q, want n1", issues[0].Tags["node"])
	}
}

func TestExceptionFallbackGroupsByTypeAndValueWithNoFingerprint(t *testing.T) {
	s := NewServer(t)
	envelope, _, key := envelopeURLs(t, s.DSN())

	payload := `{"event_id":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee","exception":{"values":[{"type":"RuntimeError","value":"podman: broken"}]}}`
	resp := post(t, envelope, eventEnvelope("eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", payload), false, key)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	issues := s.Issues()
	if len(issues) != 1 {
		t.Fatalf("Issues() = %d, want 1", len(issues))
	}
	want := "RuntimeError: podman: broken"
	if issues[0].Fingerprint != want {
		t.Errorf("Fingerprint = %q, want %q", issues[0].Fingerprint, want)
	}
}

func TestStoreEndpointAcceptsABarePayload(t *testing.T) {
	s := NewServer(t)
	_, store, key := envelopeURLs(t, s.DSN())

	payload := `{"event_id":"ffffffffffffffffffffffffffffffff","message":"legacy store"}`
	resp := post(t, store, []byte(payload), false, key)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	events := s.Events()
	if len(events) != 1 || events[0].Message != "legacy store" {
		t.Fatalf("Events() = %+v, want one event with message legacy store", events)
	}
}
