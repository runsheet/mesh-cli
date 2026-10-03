package node

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"projectmesh.io/mesh-cli/internal/gateway"
	"projectmesh.io/mesh-cli/internal/paths"
)

// PersonalNode is what `mesh mcp` saves of the personal node it
// registers (ADR-015's owner form, ADR-023 mesh mcp): its id, its
// name, and when it was first registered — so a later start reuses the
// saved id instead of making another row.
type PersonalNode struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	RegisteredAt time.Time `json:"registeredAt"`
}

// PersonalNodePath is ~/.mesh/personal-node.json.
func PersonalNodePath() (string, error) {
	dir, err := paths.MeshDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "personal-node.json"), nil
}

// LoadPersonalNode reads ~/.mesh/personal-node.json; a file that does
// not exist yet is a zero PersonalNode and no error.
func LoadPersonalNode() (PersonalNode, error) {
	path, err := PersonalNodePath()
	if err != nil {
		return PersonalNode{}, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return PersonalNode{}, nil
	}
	if err != nil {
		return PersonalNode{}, err
	}
	var pn PersonalNode
	if err := json.Unmarshal(data, &pn); err != nil {
		return PersonalNode{}, err
	}
	return pn, nil
}

// SavePersonalNode writes ~/.mesh/personal-node.json, 0600.
func SavePersonalNode(pn PersonalNode) error {
	dir, err := paths.EnsureMeshDir()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(pn, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "personal-node.json"), data, 0o600)
}

// RegisterPersonalGateway is what RegisterPersonal needs of the
// gateway: *gateway.Client has it.
type RegisterPersonalGateway interface {
	TakesOwner(ctx context.Context) bool
	TakesNodeKind(ctx context.Context) bool
	RegisterNode(ctx context.Context, in gateway.RegisterNodeInput) (*gateway.NodeAnswer, error)
}

// RegisterPersonal registers (or, on a later start, reuses) `mesh
// mcp`'s personal node: a person's node (ADR-015's owner form, sent
// only when the platform's RegisterNodeInput takes an owner field),
// kind personal (sent only when it takes kind), capacity 1, named
// "<hostname> (Claude Code)" (ADR-023, mesh mcp). The first
// registration's id is saved to ~/.mesh/personal-node.json; a later
// call reuses it as RegisterNodeInput.NodeID, so the platform updates
// the one row instead of making another.
func RegisterPersonal(ctx context.Context, gw RegisterPersonalGateway, version string) (PersonalNode, error) {
	saved, err := LoadPersonalNode()
	if err != nil {
		return PersonalNode{}, err
	}
	nodeID := saved.ID
	if nodeID == "" {
		nodeID = uuid.NewString()
	}
	hostname, err := os.Hostname()
	if err != nil {
		return PersonalNode{}, err
	}
	name := fmt.Sprintf("%s (Claude Code)", hostname)
	in := gateway.RegisterNodeInput{
		NodeID:    nodeID,
		Name:      name,
		Harnesses: []string{"claude-code"},
		Capacity:  1,
		Version:   version,
	}
	if gw.TakesOwner(ctx) {
		in.Owner = "person"
	}
	if gw.TakesNodeKind(ctx) {
		in.Kind = "personal"
	}
	answer, err := gw.RegisterNode(ctx, in)
	if err != nil {
		return PersonalNode{}, err
	}
	id := nodeID
	if answer != nil && answer.NodeID != "" {
		id = answer.NodeID
	}
	pn := PersonalNode{ID: id, Name: name, RegisteredAt: time.Now()}
	if saved.ID != "" {
		pn.RegisteredAt = saved.RegisteredAt
	}
	if err := SavePersonalNode(pn); err != nil {
		return PersonalNode{}, err
	}
	return pn, nil
}
