package channels_test

import (
	"strings"
	"testing"

	"projectmesh.io/mesh-cli/internal/channels"
)

func TestValidateSpecsChannelMode(t *testing.T) {
	specs := []channels.Spec{
		{
			Type: "discord",
			Config: map[string]any{
				"account":   "prod-bot",
				"channelId": "123456789012345678",
			},
		},
	}
	if err := channels.ValidateSpecs(specs); err != nil {
		t.Errorf("expected no error for valid channel-mode spec, got: %v", err)
	}
}

func TestValidateSpecsGuildMode(t *testing.T) {
	specs := []channels.Spec{
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
	if err := channels.ValidateSpecs(specs); err != nil {
		t.Errorf("expected no error for valid guild-mode spec, got: %v", err)
	}
}

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
			specs := []channels.Spec{{Type: "discord", Config: tc.config}}
			err := channels.ValidateSpecs(specs)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestValidateSpecsDuplicateChannelIDWithinAgent(t *testing.T) {
	specs := []channels.Spec{
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
				"channelId": "123456789012345678",
			},
		},
	}
	err := channels.ValidateSpecs(specs)
	if err == nil {
		t.Fatal("expected conflict error for duplicate channelId, got nil")
	}
	if !strings.Contains(err.Error(), "already claimed") {
		t.Errorf("error %q does not mention conflict", err.Error())
	}
}

func TestValidateSpecsGuildOverrideDuplicateWithinAgent(t *testing.T) {
	const sharedChannelID = "111111111111111111"
	specs := []channels.Spec{
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
	err := channels.ValidateSpecs(specs)
	if err == nil {
		t.Fatal("expected conflict error for override channelId matching existing channel-mode entry, got nil")
	}
	if !strings.Contains(err.Error(), "already claimed") {
		t.Errorf("error %q does not mention conflict", err.Error())
	}
}

func TestValidateWhatsappConfig(t *testing.T) {
	valid := []map[string]any{
		{"account": "personal", "chatJid": "15551234567@s.whatsapp.net"},
		{"account": "work", "chatJid": "120363000000000001@g.us"},
	}
	for _, cfg := range valid {
		specs := []channels.Spec{{Type: "whatsapp", Config: cfg}}
		if err := channels.ValidateSpecs(specs); err != nil {
			t.Errorf("expected valid, got error: %v (cfg=%v)", err, cfg)
		}
	}

	cases := []struct {
		cfg     map[string]any
		wantErr string
	}{
		{map[string]any{"chatJid": "123@s.whatsapp.net"}, "account is required"},
		{map[string]any{"account": "a"}, "chatJid is required"},
		{map[string]any{"account": "a", "chatJid": "123@invalid.domain"}, "not a valid WhatsApp JID"},
		{map[string]any{"account": "a", "chatJid": "abc@s.whatsapp.net"}, "not a valid WhatsApp JID"},
		{map[string]any{"account": "a", "chatJid": "@s.whatsapp.net"}, "not a valid WhatsApp JID"},
	}
	for _, tc := range cases {
		specs := []channels.Spec{{Type: "whatsapp", Config: tc.cfg}}
		err := channels.ValidateSpecs(specs)
		if err == nil {
			t.Errorf("expected error containing %q for cfg %v, got nil", tc.wantErr, tc.cfg)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("error %q does not contain %q (cfg=%v)", err.Error(), tc.wantErr, tc.cfg)
		}
	}
}

func TestValidateSpecsOverrideMissingChannelID(t *testing.T) {
	specs := []channels.Spec{
		{
			Type: "discord",
			Config: map[string]any{
				"account":          "prod-bot",
				"subscriptionMode": "guild",
				"guildId":          "999999999999999999",
				"channelOverrides": []any{
					map[string]any{"requireMention": false},
				},
			},
		},
	}
	err := channels.ValidateSpecs(specs)
	if err == nil {
		t.Fatal("expected error for override missing channelId, got nil")
	}
}
