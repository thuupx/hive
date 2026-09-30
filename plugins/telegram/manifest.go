package telegram

import v1 "github.com/thuupx/hive/protocol/hive/v1"

// Manifest is what this transport declares about itself.
//
// The core reads it to know which environment variables the service must
// carry and what `hive init` should scaffold. It is the transport's own
// description, so adding an option does not mean editing the core.
func Manifest(version string) v1.PluginManifest {
	return v1.PluginManifest{
		ID:      Name,
		Type:    v1.PluginTypeTransport,
		Version: version,
		Secrets: []v1.PluginSecret{
			// TELEGRAM_TOKEN is an alias for the value a .env file usually
			// holds, so either name satisfies the requirement.
			{Any: []string{"TELEGRAM_BOT_TOKEN", "TELEGRAM_TOKEN"}, Description: "the bot token, \"<bot id>:<secret>\""},
		},
		Config: &v1.PluginConfigManifest{
			Section: "transport.telegram",
			Summary: "Telegram over long polling, so no public endpoint is needed.",
			// The user id is numeric; bots like @userinfobot report it.
			PrincipalHint: "your numeric user id — @userinfobot reports it",
			Options: []v1.PluginOptionManifest{
				// The first two only matter in a group; a private chat is 1:1
				// and always addressed, so a configuration for one can leave
				// both out.
				{Name: "bot_username", Default: "", Description: "groups only: the bot's @username, for mention resolution; learned from getMe when empty"},
				{Name: "require_mention", Default: "false", Description: "groups only: ignore a message that does not address the bot"},
				{Name: "max_attachment_mb", Default: "8", Description: "largest file the transport will read"},
				{Name: "typing_indicator", Default: "true", Description: "show Telegram's typing action and a working reaction on the message while a turn runs"},
			},
			// The signal is the transient typing action rather than a mark on
			// the user's message; mode is accepted for parity and ignored.
			Acknowledgement: &v1.PluginAcknowledgementManifest{
				Enabled: true,
				Mode:    "visual",
			},
		},
	}
}
