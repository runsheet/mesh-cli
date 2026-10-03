package commands

import (
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/node"
)

// `mesh node register --owner enterprise` with no `--enterprise` is
// refused before any network call, with the same message LoadConfig
// gives a node.yaml carrying the same combination.
func TestNodeRegisterRefusesOwnerEnterpriseWithoutEnterprise(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	oldName, oldOwner, oldEnterprise := nodeNameFlag, nodeOwnerFlag, nodeEnterpriseFlag
	t.Cleanup(func() { nodeNameFlag, nodeOwnerFlag, nodeEnterpriseFlag = oldName, oldOwner, oldEnterprise })
	nodeNameFlag, nodeOwnerFlag, nodeEnterpriseFlag = "mini", "enterprise", ""

	err := nodeRegisterCmd.RunE(nodeRegisterCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "enterprise: <slug or id>") {
		t.Fatalf("err = %v, want it to name `enterprise: <slug or id>`", err)
	}
}

// An `owner: enterprise` a prior registration saved, with no
// `--enterprise` on this invocation either, is refused the same way —
// registering again must not silently drop the requirement.
func TestNodeRegisterRefusesInheritedOwnerEnterpriseWithoutEnterprise(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	prev := &node.Config{Slug: "mini", Name: "mini", Capacity: node.Fixed(1), Owner: "enterprise"}
	if err := prev.Save(); err != nil {
		t.Fatal(err)
	}
	oldName, oldOwner, oldEnterprise := nodeNameFlag, nodeOwnerFlag, nodeEnterpriseFlag
	t.Cleanup(func() { nodeNameFlag, nodeOwnerFlag, nodeEnterpriseFlag = oldName, oldOwner, oldEnterprise })
	nodeNameFlag, nodeOwnerFlag, nodeEnterpriseFlag = "mini", "", ""

	err := nodeRegisterCmd.RunE(nodeRegisterCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "enterprise: <slug or id>") {
		t.Fatalf("err = %v, want it to name `enterprise: <slug or id>`", err)
	}
}

// `--enterprise` is carried into the flag merge ahead of any network
// call: given with `--owner enterprise`, ValidateOwner passes and the
// command proceeds past the early refusal (it then fails on the
// network call this test gives no credential for, proving the
// validation step let it through rather than stopping it itself).
func TestNodeRegisterAcceptsOwnerEnterpriseWithEnterprise(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	oldName, oldOwner, oldEnterprise := nodeNameFlag, nodeOwnerFlag, nodeEnterpriseFlag
	t.Cleanup(func() { nodeNameFlag, nodeOwnerFlag, nodeEnterpriseFlag = oldName, oldOwner, oldEnterprise })
	nodeNameFlag, nodeOwnerFlag, nodeEnterpriseFlag = "mini", "enterprise", "anythink"

	err := nodeRegisterCmd.RunE(nodeRegisterCmd, nil)
	if err == nil || strings.Contains(err.Error(), "enterprise: <slug or id>") || strings.Contains(err.Error(), "owner: is not enterprise") {
		t.Fatalf("err = %v, want it to pass validation and fail later (no credential), not on the owner/enterprise check", err)
	}
}
