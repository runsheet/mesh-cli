// Package triplinetest fakes Tripline's ingest endpoint for tests: it
// accepts the Sentry envelope protocol Tripline speaks, parses events out
// of it, and groups them into issues by fingerprint.
package triplinetest

import (
	"bytes"
	"encoding/json"
	"strings"
)

// envelopeItem is one item of an envelope: its header and its raw payload.
type envelopeItem struct {
	Header  map[string]any
	Payload []byte
}

// parseEnvelope splits a Sentry envelope into its header and items.
//
// An envelope is newline-delimited: an envelope header, then for each item
// an item header followed by its payload. When an item header carries a
// "length" the payload is exactly that many bytes (which may itself
// contain newlines); otherwise the payload runs to the next newline.
func parseEnvelope(body []byte) (map[string]any, []envelopeItem, error) {
	headerLine, pos := readLine(body, 0)
	var header map[string]any
	if err := json.Unmarshal(headerLine, &header); err != nil {
		return nil, nil, err
	}

	var items []envelopeItem
	for pos < len(body) {
		itemHeaderLine, next := readLine(body, pos)
		if len(bytes.TrimSpace(itemHeaderLine)) == 0 {
			break
		}
		pos = next

		var itemHeader map[string]any
		if err := json.Unmarshal(itemHeaderLine, &itemHeader); err != nil {
			return nil, nil, err
		}

		var payload []byte
		if lengthRaw, ok := itemHeader["length"]; ok {
			length, _ := lengthRaw.(float64)
			n := int(length)
			if n < 0 || pos+n > len(body) {
				n = len(body) - pos
			}
			payload = body[pos : pos+n]
			pos += n
			if pos < len(body) && body[pos] == '\n' {
				pos++
			}
		} else {
			payload, pos = readLine(body, pos)
		}

		items = append(items, envelopeItem{Header: itemHeader, Payload: payload})
	}

	return header, items, nil
}

// readLine returns the bytes of data from pos up to (not including) the
// next newline, and the position just past that newline (or the end of
// data when there is none).
func readLine(data []byte, pos int) ([]byte, int) {
	idx := bytes.IndexByte(data[pos:], '\n')
	if idx == -1 {
		return data[pos:], len(data)
	}
	return data[pos : pos+idx], pos + idx + 1
}

// rawEvent is the Sentry event payload shape, parsed loosely: only the
// fields Tripline groups and tags issues on.
type rawEvent struct {
	EventID     string            `json:"event_id"`
	Message     json.RawMessage   `json:"message"`
	Exception   json.RawMessage   `json:"exception"`
	Fingerprint []string          `json:"fingerprint"`
	Tags        map[string]string `json:"tags"`
	Level       string            `json:"level"`
}

// parseEventPayload reads an event item's payload into an Event.
func parseEventPayload(payload []byte) (Event, error) {
	var raw rawEvent
	if err := json.Unmarshal(payload, &raw); err != nil {
		return Event{}, err
	}

	excType, excValue := parseException(raw.Exception)

	return Event{
		EventID:        raw.EventID,
		Message:        parseMessage(raw.Message),
		ExceptionType:  excType,
		ExceptionValue: excValue,
		Fingerprint:    raw.Fingerprint,
		Tags:           raw.Tags,
		Level:          raw.Level,
		Payload:        append([]byte(nil), payload...),
	}, nil
}

// parseMessage reads the Sentry "message" interface: either a bare string,
// or an object carrying "formatted" (preferred) or "message".
func parseMessage(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var obj struct {
		Formatted string `json:"formatted"`
		Message   string `json:"message"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		if obj.Formatted != "" {
			return obj.Formatted
		}
		return obj.Message
	}
	return ""
}

// parseException reads the Sentry "exception" interface, `{"values":
// [{"type", "value"}, ...]}` or the bare list sentry-go sends, and
// returns the first value's type and value.
func parseException(raw json.RawMessage) (excType, excValue string) {
	if len(raw) == 0 {
		return "", ""
	}
	var wrapped struct {
		Values []struct {
			Type  string `json:"type"`
			Value string `json:"value"`
		} `json:"values"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil && len(wrapped.Values) > 0 {
		return wrapped.Values[0].Type, wrapped.Values[0].Value
	}
	var list []struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(raw, &list); err == nil && len(list) > 0 {
		return list[0].Type, list[0].Value
	}
	return "", ""
}

// fingerprintKey is how Tripline groups events into issues: the event's
// own fingerprint when set, else its exception's type and value, else its
// message.
func fingerprintKey(e Event) string {
	if len(e.Fingerprint) > 0 {
		return strings.Join(e.Fingerprint, "|")
	}
	if e.ExceptionType != "" || e.ExceptionValue != "" {
		return e.ExceptionType + ": " + e.ExceptionValue
	}
	return e.Message
}
