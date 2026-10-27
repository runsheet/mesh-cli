package node

import (
	"context"

	"projectmesh.io/mesh-cli/internal/gateway"
)

// briefOf and kindOf pass a wrapped gateway's briefs and catalogue
// through, so an evaluation started on a test's recorder reads its
// kind's finish tool as it would on the client.
func briefOf(gw Gateway, kind string, subject gateway.JobSubject) (*gateway.JobBrief, bool) {
	if r, ok := gw.(briefReader); ok {
		return r.JobBriefOf(kind, subject)
	}
	return nil, false
}

func kindOf(ctx context.Context, gw Gateway, kind string) (gateway.JobKind, bool) {
	if r, ok := gw.(kindReader); ok {
		return r.JobKindOf(ctx, kind)
	}
	return gateway.JobKind{}, false
}

func (g *beatGateway) JobBriefOf(kind string, s gateway.JobSubject) (*gateway.JobBrief, bool) {
	return briefOf(g.Gateway, kind, s)
}
func (g *beatGateway) JobKindOf(ctx context.Context, kind string) (gateway.JobKind, bool) {
	return kindOf(ctx, g.Gateway, kind)
}
func (g *tokenGW) JobBriefOf(kind string, s gateway.JobSubject) (*gateway.JobBrief, bool) {
	return briefOf(g.Gateway, kind, s)
}
func (g *tokenGW) JobKindOf(ctx context.Context, kind string) (gateway.JobKind, bool) {
	return kindOf(ctx, g.Gateway, kind)
}
func (g *modelGW) JobBriefOf(kind string, s gateway.JobSubject) (*gateway.JobBrief, bool) {
	return briefOf(g.Gateway, kind, s)
}
func (g *modelGW) JobKindOf(ctx context.Context, kind string) (gateway.JobKind, bool) {
	return kindOf(ctx, g.Gateway, kind)
}
func (g *evalRecorder) JobBriefOf(kind string, s gateway.JobSubject) (*gateway.JobBrief, bool) {
	return briefOf(g.Gateway, kind, s)
}
func (g *evalRecorder) JobKindOf(ctx context.Context, kind string) (gateway.JobKind, bool) {
	return kindOf(ctx, g.Gateway, kind)
}
