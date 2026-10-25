package node

import (
	"context"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// handInRefused logs a hand-in the gateway refused — `item <label>: <call>
// refused: <err>`, the gateway's answer as it gave it, redacted — and
// returns err unchanged, so the caller's own handling of the refusal goes
// on as before. A nil err logs nothing. Called on what is left once the
// usage report's resend-without-a-field answers are absorbed (Carried,
// validationUsage), so only a finish the platform did not take is logged.
func handInRefused(logf func(string, ...any), redact func(string) string, label, call string, err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if redact != nil {
		msg = redact(msg)
	}
	if logf != nil {
		logf("item %s: %s refused: %s", label, call, msg)
	}
	return err
}

// itemLabelOf is the `<goal>/<seq>` label RunItem put on ctx
// (WithLockHolder), else the item's id.
func itemLabelOf(ctx context.Context, item gateway.WorkItem) string {
	if l, ok := ctx.Value(lockHolderKey{}).(string); ok && l != "" {
		return l
	}
	return item.ID
}
