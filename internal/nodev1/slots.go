package nodev1

import (
	"context"
	"sort"
	"strings"

	"projectmesh.io/mesh-cli/internal/mcptool"
	"projectmesh.io/mesh-cli/internal/meshapi"
	"projectmesh.io/mesh-cli/internal/relaysidecar"
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
//   - from connection: a slot an org or project fills (projectmesh-io/api
//     ADR 001 amendment, slot bindings). The take resolves the tenant's
//     binding at Latchkey and the brief names its ref; the slot is then
//     served as an MCP server of its own, named the slot, proxied to
//     Latchkey's {latchkey}/v1/connections/for-job/{binding}/mcp on the
//     lease's session token (sessionproxy.Slot), which adds the
//     connection's credential and presents the vendor's bound tool under
//     the slot's name. The relay holds it to that one tool with the
//     kind's description, so swapping the vendor changes nothing the
//     model sees. An optional slot with no binding is not served; a
//     required one hands the lease back tool_unavailable before anything
//     is minted.
//
// The lease's usage records, by name alone, every tool it was served (a
// tool count with no calls is one served and never called) beside the
// calls the stream counted, a slot's with its slot and binding.

// slots are a brief's tools as the node serves them: the product tools
// and node tools by name, each to its description.
type slots struct {
	product sessionproxy.ToolFilter
	mesh    map[string]string
	// filled are the connection slots a binding fills, in the brief's
	// order.
	filled []filledSlot
	// proxy is the lease's sessionproxy, once open: what it presented.
	proxy *sessionproxy.Proxy
	// nodeServed are the node tools the session was served.
	nodeServed []string
}

// filledSlot is a connection slot a binding fills: the slot's name (its
// server's and its tool's), the kind's description and the binding's
// ref.
type filledSlot struct {
	name, description, binding string
}

// slotsOf are b's slots, nil when its kind names no tools. A required
// tool the node cannot serve — a connection slot no binding fills, or a
// source it does not know — is a *toolUnavailableError, as is a filled
// slot whose name is already a server's.
func slotsOf(b *meshapi.Brief) (*slots, error) {
	if b.Tools == nil {
		return nil, nil
	}
	s := &slots{product: sessionproxy.ToolFilter{}, mesh: map[string]string{}}
	taken := map[string]bool{NodeToolsServer: true}
	for _, srv := range b.Servers {
		taken[srv.Name] = true
	}
	fill := func(t meshapi.SlotTool) error {
		if taken[t.Name] {
			return &toolUnavailableError{tool: t.Name, why: "a connection slot named as one of the session's servers already is"}
		}
		taken[t.Name] = true
		s.filled = append(s.filled, filledSlot{name: t.Name, description: t.Description, binding: t.Binding})
		return nil
	}
	for _, t := range b.Tools.Required {
		switch t.From {
		case meshapi.ToolFromProduct:
			s.product[t.Name] = t.Description
		case meshapi.ToolFromMesh:
			s.mesh[t.Name] = t.Description
		case meshapi.ToolFromConnection:
			if t.Binding == "" {
				return nil, &toolUnavailableError{tool: t.Name, why: "a connection slot no connection of the tenant's fills"}
			}
			if err := fill(t); err != nil {
				return nil, err
			}
		default:
			return nil, &toolUnavailableError{tool: t.Name, why: "a tool from " + t.From + ", which this node does not serve"}
		}
	}
	// optional tools are connection slots, served once the tenant fills
	// them: an unfilled one is not served
	for _, t := range b.Tools.Optional {
		if t.From != meshapi.ToolFromConnection || t.Binding == "" {
			continue
		}
		if err := fill(t); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// hasConnections says the lease serves a filled slot, which it reaches
// on its Latchkey session.
func (s *slots) hasConnections() bool { return s != nil && len(s.filled) > 0 }

// connectionUpstreams are the filled slots' servers, by slot name, each
// Latchkey's proxy for its binding at issuer on the lease's session
// token.
func (s *slots) connectionUpstreams(issuer string, bearer func(context.Context) (string, error), key sessionproxy.Signer) map[string]sessionproxy.Upstream {
	if s == nil {
		return nil
	}
	out := map[string]sessionproxy.Upstream{}
	for _, f := range s.filled {
		out[f.name] = sessionproxy.Slot{URL: strings.TrimRight(issuer, "/") + relaysidecar.SlotPath(f.binding), Bearer: bearer, Key: key,
			Tool: f.name, Description: f.description}
	}
	return out
}

// sidecarSlots are the filled slots as a relay sidecar's credentials
// carry them: names, descriptions and refs, never a credential.
func (s *slots) sidecarSlots() []relaysidecar.Slot {
	if s == nil {
		return nil
	}
	var out []relaysidecar.Slot
	for _, f := range s.filled {
		out = append(out, relaysidecar.Slot{Name: f.name, Description: f.description, Binding: f.binding})
	}
	return out
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
	// a filled slot's tool, as the harness names it, to its slot
	bySlot := map[string]filledSlot{}
	for _, f := range s.filled {
		bySlot["mcp__"+f.name+"__"+f.name] = f
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
	for i := range out {
		if f, ok := bySlot[out[i].Tool]; ok {
			out[i].Slot, out[i].Binding = f.name, f.binding
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tool < out[j].Tool })
	return out
}
