package slack

import v1 "github.com/thuupx/hive/protocol/hive/v1"

// Manifest is what this transport declares about itself.
//
// The core reads it to know which environment variables the service must carry
// and what `hive init` should scaffold. It is the transport's own description, so
// adding an option does not mean editing the core.
func Manifest(version string) v1.PluginManifest {
	return v1.PluginManifest{
		ID:      Name,
		Type:    v1.PluginTypeTransport,
		Version: version,
		Secrets: []v1.PluginSecret{
			{Any: []string{"SLACK_APP_TOKEN"}, Description: "the Socket Mode connection token"},
			{Any: []string{"SLACK_BOT_TOKEN"}, Description: "the Web API token"},
		},
		Config: &v1.PluginConfigManifest{
			Section: "transport.slack",
			Summary: "Slack over Socket Mode, so no public endpoint is needed.",
			// A member id looks like U0123456789; a profile's ⋯ menu copies it.
			PrincipalHint: "your member id (U…) — profile → ⋯ → Copy member ID",
			Options: []v1.PluginOptionManifest{
				{
					Name:        "bot_user_id",
					Default:     "U0XXXXXXX",
					Description: "the bot's own user id, for mention resolution",
					// Without it a mention is never recognised, and with
					// require_mention on, every message is ignored.
					Required: true,
				},
				{Name: "require_mention", Default: "true", Description: "ignore messages that do not address the bot"},
				{Name: "channel_context", Default: "20", Description: "recent messages handed to the agent as room context"},
				{Name: "max_attachment_mb", Default: "8", Description: "largest file the transport will read"},
				{Name: "thread_replies", Default: "true", Description: "thread a channel turn under the message that asked for it"},
				{Name: "typing_indicator", Default: "true", Description: "show a turn is working by animating a message"},
			},
			Acknowledgement: &v1.PluginAcknowledgementManifest{
				Enabled:  true,
				Mode:     "reaction",
				Reaction: "eyes",
			},
		},
	}
}
