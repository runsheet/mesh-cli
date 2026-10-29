package nodev1

import (
	"fmt"
	"sort"
	"strings"

	"projectmesh.io/mesh-cli/internal/meshapi"
)

// Prompt is a lease's prompt: the kind's guidance, as the product wrote
// it, then the job's facts and how it reaches its product. Mesh carries
// no content of the work itself; the session reads it with the product's
// brief tool.
func Prompt(b *meshapi.Brief) string {
	var sb strings.Builder
	if g := strings.TrimSpace(b.Guidance); g != "" {
		sb.WriteString(g)
		sb.WriteString("\n\n")
	}
	sb.WriteString("## This job\n\n")
	fmt.Fprintf(&sb, "You are running job %s, a %s job for %s (tenant %s), about %s %s.\n",
		b.JobID, b.Kind, b.Product, b.Tenant, b.Subject.Type, b.Subject.ID)
	if b.Handbacks > 0 {
		fmt.Fprintf(&sb, "It was handed back %d time(s) before; start from what the product's brief says now.\n", b.Handbacks)
	}
	names := make([]string, 0, len(b.Servers))
	for _, s := range b.Servers {
		names = append(names, s.Name)
	}
	sort.Strings(names)
	if len(names) > 0 {
		fmt.Fprintf(&sb, "The product's tools are on the MCP server(s) %s.\n", strings.Join(names, ", "))
	}
	if b.BriefTool != "" {
		fmt.Fprintf(&sb, "Start by calling `%s` for the work.\n", b.BriefTool)
	}
	if b.FinishTool != "" {
		fmt.Fprintf(&sb, "When the work is done, call `%s`", b.FinishTool)
		if len(b.FinishSchema) > 0 {
			fmt.Fprintf(&sb, " with an input matching this JSON schema: %s", strings.TrimSpace(string(b.FinishSchema)))
		}
		sb.WriteString(", then stop.\n")
	}
	if b.Code != nil && b.Code.Repo.FullName != "" {
		fmt.Fprintf(&sb, "Your working directory is a checkout of %s at %s.\n", b.Code.Repo.FullName, shortSHA(b.Code.BaseSHA))
	}
	return sb.String()
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
