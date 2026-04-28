package main

// TODO(spec22-phase5): WhatsApp account state I/O moved to internal/accounts.
// This file is a shim; symbols are re-exposed here for callers in cmd/mesh
// that haven't been migrated yet (localsock.go, whatsappcmd.go, channelmgr.go).

import (
	"net"
	"time"

	"projectmesh.io/mesh-cli/internal/accounts"
	"projectmesh.io/mesh-cli/internal/paths"
)

type whatsappAccountMeta = accounts.WhatsappAccountMeta

func WriteWhatsappAccount(name string, meta whatsappAccountMeta) error {
	return accounts.WriteWhatsappAccount(name, meta)
}

func LoadWhatsappAccountMeta(name string) (whatsappAccountMeta, error) {
	return accounts.LoadWhatsappAccountMeta(name)
}

func UpdateWhatsappAccountMeta(name string, meta whatsappAccountMeta) error {
	return accounts.UpdateWhatsappAccountMeta(name, meta)
}

func ListWhatsappAccounts() ([]string, error) {
	return accounts.ListWhatsappAccounts()
}

func RemoveWhatsappAccount(name string) (bool, error) {
	return accounts.RemoveWhatsappAccount(name)
}

func WhatsappGatewaySockPath(name string) (string, error) {
	return paths.WhatsappGatewaySockPath(name)
}

func WhatsappStorePath(name string) (string, error) {
	return accounts.WhatsappStorePath(name)
}

func agentReferencesWhatsappAccount(accountName string) (bool, string, error) {
	agents, err := ListAgents()
	if err != nil {
		return false, "", err
	}
	for _, a := range agents {
		for _, ch := range a.Channels {
			if ch.Type != "whatsapp" {
				continue
			}
			acc, _ := ch.Config["account"].(string)
			if acc == accountName {
				return true, a.Name, nil
			}
		}
	}
	return false, "", nil
}

func whatsappGatewayLive(account string) bool {
	sockPath, err := WhatsappGatewaySockPath(account)
	if err != nil {
		return false
	}
	conn, err := net.DialTimeout("unix", sockPath, 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
