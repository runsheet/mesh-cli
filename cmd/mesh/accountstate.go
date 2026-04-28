package main

// TODO(spec22-phase5): account state I/O moved to internal/accounts.
// This file is a shim; symbols are re-exposed here for callers in cmd/mesh
// that haven't been migrated yet (localsock.go, discordcmd.go, channelmgr.go).

import (
	"projectmesh.io/mesh-cli/internal/accounts"
	"projectmesh.io/mesh-cli/internal/paths"
)

func platformAccountDir(platform, name string) (string, error) {
	return paths.PlatformAccountDir(platform, name)
}

type discordAccountMeta = accounts.DiscordAccountMeta
type GatewayStatus = accounts.GatewayStatus
type GatewaySubStatus = accounts.GatewaySubStatus

func WriteDiscordAccount(name, token string) error {
	return accounts.WriteDiscordAccount(name, token)
}

func RemoveDiscordAccount(name string) (bool, error) {
	return accounts.RemoveDiscordAccount(name)
}

func LoadDiscordToken(name string) (string, error) {
	return accounts.LoadDiscordToken(name)
}

func LoadDiscordAccountMeta(name string) (discordAccountMeta, error) {
	return accounts.LoadDiscordAccountMeta(name)
}

func UpdateDiscordAccountMeta(name string, meta discordAccountMeta) error {
	return accounts.UpdateDiscordAccountMeta(name, meta)
}

func ListDiscordAccounts() ([]string, error) {
	return accounts.ListDiscordAccounts()
}

func DiscordGatewaySockPath(accountName string) (string, error) {
	return paths.DiscordGatewaySockPath(accountName)
}

func WriteGatewayStatus(platform, accountName string, st GatewayStatus) error {
	return accounts.WriteGatewayStatus(platform, accountName, st)
}

func ReadGatewayStatus(platform, accountName string) (GatewayStatus, error) {
	return accounts.ReadGatewayStatus(platform, accountName)
}

func agentReferencesDiscordAccount(accountName string) (bool, string, error) {
	agents, err := ListAgents()
	if err != nil {
		return false, "", err
	}
	for _, a := range agents {
		for _, ch := range a.Channels {
			if ch.Type != "discord" {
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
