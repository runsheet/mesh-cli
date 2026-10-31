package meshapi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// The event types a node's stream carries (Mesh v1 spec, "Node API":
// SSE carrying cancel, turn, pause and drain).
const (
	EventCancel = "cancel"
	EventTurn   = "turn"
	EventPause  = "pause"
	EventDrain  = "drain"
)

// NodeEvent is one server-sent event of the node's stream: its type and
// its JSON data.
type NodeEvent struct {
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data"`
}

// CancelEvent is a cancel's data: stop the lease's session and pods and
// hand it back with CauseCancelled. Cause is why Mesh cancelled the job
// (cancelled, superseded).
type CancelEvent struct {
	LeaseID string `json:"lease_id"`
	JobID   string `json:"job_id"`
	Cause   string `json:"cause"`
	Reason  string `json:"reason,omitempty"`
}

// TurnEvent is a turn's data: a live job's new turn, for the lease's
// session (Mesh v1 spec, "Threads carry no content": ids only; the
// session reads the turn with the product's turn tool), with its kind
// and who it is from (api ADR 001 amendment, threads: "Turns into any
// live job").
type TurnEvent struct {
	LeaseID string `json:"lease_id"`
	JobID   string `json:"job_id"`
	TurnID  string `json:"turn_id"`
	Seq     int64  `json:"seq"`
	Kind    string `json:"kind,omitempty"`
	From    string `json:"from,omitempty"`
}

// maxEventLine bounds one line of the stream.
const maxEventLine = 1 << 20

// Events reads the node's event stream, GET /v1/nodes/{id}/events,
// handing each event to on until the stream ends or ctx does. A clean
// end of the stream (Mesh restarting, a proxy's timeout) answers nil:
// the caller connects again. A refusal is an *Error. The stream waits
// as long as Mesh keeps it open, so it does not go on the client's own
// timeout.
func (c *Client) Events(ctx context.Context, nodeID string, on func(NodeEvent)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base()+"/v1/nodes/"+url.PathEscape(nodeID)+"/events", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	if c.Token != nil {
		tok, err := c.Token(ctx)
		if err != nil {
			return fmt.Errorf("mesh: bearer: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	hc := *c.client()
	hc.Timeout = 0
	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		e := &Error{Status: resp.StatusCode}
		if json.Unmarshal(raw, e) != nil || (e.Code == "" && e.Message == "") {
			e.Message = strings.TrimSpace(string(raw))
		}
		return e
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		return fmt.Errorf("mesh: the event stream answered %q, not text/event-stream", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), maxEventLine)
	var ev NodeEvent
	var data []string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if ev.Event != "" || len(data) > 0 {
				if ev.Event == "" {
					ev.Event = "message"
				}
				ev.Data = json.RawMessage(strings.Join(data, "\n"))
				on(ev)
			}
			ev, data = NodeEvent{}, nil
		case strings.HasPrefix(line, ":"):
			// a comment: the stream's keep-alive
		default:
			field, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "event":
				ev.Event = value
			case "data":
				data = append(data, value)
			}
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}
