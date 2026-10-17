package gateway_test

import (
	"context"
	"testing"
	"time"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// A heartbeat carries the quartermaster's findings, every field of each,
// when the gateway's HeartbeatNodeInput takes `findings` (goal
// the-quartermaster-keeps-the-nodes-fit); a schema without it gets no
// `findings` key and the heartbeat lands the first time.
func TestHeartbeatCarriesFindingsTheSchemaTakes(t *testing.T) {
	at := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	findings := []gateway.Finding{{Kind: "scratch", Severity: "warn", Subject: "/tmp", Detail: "reaped 3 dirs, 2.0G; 1 in use kept", Bytes: 25 << 30, Reclaimed: 2 << 30, At: at}}
	for _, takes := range []bool{true, false} {
		f, gw := registeredFake(t)
		if !takes {
			f.UnknownFields["findings"] = true
		}
		gw.Now = func() time.Time { return at }
		if _, err := gw.HeartbeatNode(context.Background(), limitsNode, "v0.2.188", 1, gateway.HeartbeatExtras{Findings: findings}); err != nil {
			t.Fatalf("takes=%v: %v", takes, err)
		}
		hb := f.CallsNamed("heartbeatNode")
		if len(hb) != 1 {
			t.Fatalf("takes=%v: heartbeats %+v (refused and resent?)", takes, hb)
		}
		v, ok := hb[0].Input["findings"]
		if !takes {
			if ok {
				t.Fatalf("sent findings the schema lacks: %#v", v)
			}
			continue
		}
		rows, _ := v.([]any)
		if len(rows) != 1 {
			t.Fatalf("findings = %#v", v)
		}
		row := rows[0].(map[string]any)
		want := map[string]any{"kind": "scratch", "severity": "warn", "subject": "/tmp", "detail": "reaped 3 dirs, 2.0G; 1 in use kept",
			"bytes": float64(25 << 30), "reclaimed": float64(2 << 30), "at": "2026-10-17T09:00:00Z"}
		if len(row) != len(want) {
			t.Fatalf("finding fields = %#v, want %#v", row, want)
		}
		for k, w := range want {
			if row[k] != w {
				t.Fatalf("finding %s = %#v, want %#v", k, row[k], w)
			}
		}
		if !gw.TakesHeartbeatFindings(context.Background()) {
			t.Fatal("TakesHeartbeatFindings false on a schema with findings")
		}
	}
}
