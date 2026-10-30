package nodev1

import (
	"sort"

	"projectmesh.io/mesh-cli/internal/mcptool"
	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/sessionproxy"
)

// A kind's tool slots (projectmesh-io/api ADR 001 amendment, tool slots
// and egress; mesh-specs gaps 6 and 10). A brief with no tools is served
// as before: every tool of the product's MCP servers and every node tool
// the kind names. A brief with tools starts the session with exactly its
// required tools and the optional slots the tenant has filled, each under
// the brief's name and description, and nothing else:
//
//   - from product: the product's MCP server's tool of that name, the
//     server's list filtered to these names and their descriptions
//     replaced by the kind's; a call of any other tool is refused in the
//     sessionproxy (sessionproxy.ToolFilter) and never reaches the
//     product;
//   - from mesh: the node tool of that name, which node_tools also names;
//   - from connection: a slot an org or project fills. Mesh has no
//     tenant slot bindings yet, so no slot is ever filled: an optional
//     one is not served, and a required one (which the publish refuses)
//     hands the lease back tool_unavailable before anything is minted.
//
// The lease's usage records, by name alone, every tool it was served (a
// tool count with no calls is one served and never called) beside the
// calls the stream counted.

// slots are a brief's tools as the node serves them: the product tools
// and node tools by name, each to its description.
type slots struct {
	product sessionproxy.ToolFilter
	mesh    map[string]string
	// proxy is the lease's sessionproxy, once open: what it presented.
	proxy *sessionproxy.Proxy
	// nodeServed are the node tools the session was served.
	nodeServed []string
}

// slotsOf are b's slots, nil when its kind names no tools. A required
// tool the node cannot serve — a connection slot, which nothing fills
// yet, or a source it does not know — is a *toolUnavailableError.
func slotsOf(b *meshapi.Brief) (*slots, error) {
	if b.Tools == nil {
		return nil, nil
	}
	s := &slots{product: sessionproxy.ToolFilter{}, mesh: map[string]string{}}
	for _, t := range b.Tools.Required {
		switch t.From {
		case meshapi.ToolFromProduct:
			s.product[t.Name] = t.Description
		case meshapi.ToolFromMesh:
			s.mesh[t.Name] = t.Description
		case meshapi.ToolFromConnection:
			return nil, &toolUnavailableError{tool: t.Name, why: "a connection slot, which no connection fills on this node"}
		default:
			return nil, &toolUnavailableError{tool: t.Name, why: "a tool from " + t.From + ", which this node does not serve"}
		}
	}
	// optional tools are connection slots, served once the tenant fills
	// them: nothing fills one yet
	return s, nil
}

// nodeTools are tools held to the slots: the node tools the brief names
// from mesh, each under its description. nil s is tools as they are.
func (s *slots) nodeTools(tools []mcptool.Tool) []mcptool.Tool {
	if s == nil {
		return tools
	}
	var out []mcptool.Tool
	s.nodeServed = nil
	for _, t := range tools {
		desc, ok := s.mesh[t.Name]
		if !ok {
			continue
		}
		t.Description = desc
		out = append(out, t)
		s.nodeServed = append(s.nodeServed, t.Name)
	}
	return out
}

// filter is the sessionproxy's product tool filter, nil when the kind
// names no tools.
func (s *slots) filter() sessionproxy.ToolFilter {
	if s == nil {
		return nil
	}
	return s.product
}

// usage adds to counted, the tools the session's stream counted, every
// tool the session was served and never called, with no calls: names as
// the harness names an MCP tool (mcp__<server>__<tool>). nil s adds
// nothing.
func (s *slots) usage(counted []meshapi.ToolCount) []meshapi.ToolCount {
	if s == nil {
		return counted
	}
	seen := map[string]bool{}
	for _, c := range counted {
		seen[c.Tool] = true
	}
	out := append([]meshapi.ToolCount(nil), counted...)
	add := func(server, name string) {
		full := "mcp__" + server + "__" + name
		if !seen[full] {
			seen[full] = true
			out = append(out, meshapi.ToolCount{Tool: full})
		}
	}
	if s.proxy != nil {
		for server, names := range s.proxy.ServedTools() {
			for _, n := range names {
				add(server, n)
			}
		}
	}
	for _, n := range s.nodeServed {
		add(NodeToolsServer, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tool < out[j].Tool })
	return out
}
