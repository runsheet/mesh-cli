package main

import (
	"strings"
	"testing"
)

// TestValidateChannelSpecsChannelMode verifies that the existing channel-mode
// template shape still parses without error.
func TestValidateChannelSpecsChannelMode(t *testing.T) {
	specs := []ChannelSpec{
		{
			Type: "discord",
			Config: map[string]any{
				"account":   "prod-bot",
				"channelId": "123456789012345678",
			},
		},
	}
	if err := validateChannelSpecs(specs); err != nil {
		t.Errorf("expected no error for valid channel-mode spec, got: %v", err)
	}
}

// TestValidateChannelSpecsGuildMode verifies that a valid guild-mode template
// parses without error.
func TestValidateChannelSpecsGuildMode(t *testing.T) {
	specs := []ChannelSpec{
		{
			Type: "discord",
			Config: map[string]any{
				"account":          "prod-bot",
				"subscriptionMode": "guild",
				"guildId":          "987654321098765432",
				"requireMention":   true,
				"channelOverrides": []any{
					map[string]any{
						"channelId":      "111111111111111111",
						"requireMention": false,
					},
					map[string]any{
						"channelId":     "222222222222222222",
						"inboundPrefix": "announcement",
					},
				},
			},
		},
	}
	if err := validateChannelSpecs(specs); err != nil {
		t.Errorf("expected no error for valid guild-mode spec, got: %v", err)
	}
}

// TestValidateDiscordMixedModeRejected verifies that channel-mode config cannot
// carry guildId, and guild-mode config cannot carry channelId.
func TestValidateDiscordMixedModeRejected(t *testing.T) {
	cases := []struct {
		name   string
		config map[string]any
		want   string
	}{
		{
			name: "channel-mode with guildId",
			config: map[string]any{
				"account":   "prod-bot",
				"channelId": "123",
				"guildId":   "456",
			},
			want: "guildId is not allowed in channel mode",
		},
		{
			name: "channel-mode with channelOverrides",
			config: map[string]any{
				"account":          "prod-bot",
				"channelId":        "123",
				"channelOverrides": []any{},
			},
			want: "channelOverrides is not allowed in channel mode",
		},
		{
			name: "guild-mode with channelId",
			config: map[string]any{
				"account":          "prod-bot",
				"subscriptionMode": "guild",
				"guildId":          "456",
				"channelId":        "123",
			},
			want: "channelId is not allowed in guild mode",
		},
		{
			name: "guild-mode missing guildId",
			config: map[string]any{
				"account":          "prod-bot",
				"subscriptionMode": "guild",
			},
			want: "guildId is required in guild mode",
		},
		{
			name: "channel-mode missing channelId",
			config: map[string]any{
				"account": "prod-bot",
			},
			want: "channelId is required in channel mode",
		},
		{
			name:   "missing account",
			config: map[string]any{"channelId": "123"},
			want:   "account is required",
		},
		{
			name: "unknown subscriptionMode",
			config: map[string]any{
				"account":          "prod-bot",
				"subscriptionMode": "all",
				"channelId":        "123",
			},
			want: "subscriptionMode must be",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			specs := []ChannelSpec{{Type: "discord", Config: tc.config}}
			err := validateChannelSpecs(specs)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

// TestValidateChannelSpecsDuplicateChannelIDWithinAgent verifies that two
// channel entries within the same agent cannot claim the same channelId.
func TestValidateChannelSpecsDuplicateChannelIDWithinAgent(t *testing.T) {
	specs := []ChannelSpec{
		{
			Type: "discord",
			Config: map[string]any{
				"account":   "prod-bot",
				"channelId": "123456789012345678",
			},
		},
		{
			Type: "discord",
			Config: map[string]any{
				"account":   "prod-bot",
				"channelId": "123456789012345678", // duplicate
			},
		},
	}
	err := validateChannelSpecs(specs)
	if err == nil {
		t.Fatal("expected conflict error for duplicate channelId, got nil")
	}
	if !strings.Contains(err.Error(), "already claimed") {
		t.Errorf("error %q does not mention conflict", err.Error())
	}
}

// TestValidateChannelSpecsGuildOverrideDuplicateWithinAgent verifies that a
// guild-mode override channelId cannot conflict with a channel-mode entry on
// the same agent.
func TestValidateChannelSpecsGuildOverrideDuplicateWithinAgent(t *testing.T) {
	const sharedChannelID = "111111111111111111"
	specs := []ChannelSpec{
		{
			Type: "discord",
			Config: map[string]any{
				"account":   "prod-bot",
				"channelId": sharedChannelID,
			},
		},
		{
			Type: "discord",
			Config: map[string]any{
				"account":          "prod-bot",
				"subscriptionMode": "guild",
				"guildId":          "999999999999999999",
				"channelOverrides": []any{
					map[string]any{"channelId": sharedChannelID},
				},
			},
		},
	}
	err := validateChannelSpecs(specs)
	if err == nil {
		t.Fatal("expected conflict error for override channelId matching existing channel-mode entry, got nil")
	}
	if !strings.Contains(err.Error(), "already claimed") {
		t.Errorf("error %q does not mention conflict", err.Error())
	}
}

// TestValidateChannelSpecsOverrideMissingChannelID verifies that override
// entries without channelId are rejected.
func TestValidateChannelSpecsOverrideMissingChannelID(t *testing.T) {
	specs := []ChannelSpec{
		{
			Type: "discord",
			Config: map[string]any{
				"account":          "prod-bot",
				"subscriptionMode": "guild",
				"guildId":          "999999999999999999",
				"channelOverrides": []any{
					map[string]any{"requireMention": false}, // missing channelId
				},
			},
		},
	}
	err := validateChannelSpecs(specs)
	if err == nil {
		t.Fatal("expected error for override missing channelId, got nil")
	}
}
