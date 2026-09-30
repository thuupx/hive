package v1

// PluginManifest is what a plugin declares about itself.
//
// A plugin is a separate process the core discovers by name, so the core knows
// nothing about it. The manifest is how a plugin says what it needs — the
// environment variables a service must carry, and the configuration section it
// reads — without the core having to learn about each plugin in turn.
//
// It lives in the protocol package because a plugin builds it and the core reads
// it, and a plugin does not link the core.
type PluginManifest struct {
	// ID is the plugin's stable identity, and the name the core knows it by.
	ID string `json:"id"`

	Type    PluginType `json:"type"`
	Version string     `json:"version"`

	// Secrets are the credentials the plugin needs.
	//
	// They are never written to configuration: `hive service install` captures
	// them into a file only its owner can read, and `hive doctor` checks they are
	// present. Naming them here is what keeps the core from having to know which
	// plugin needs which token.
	Secrets []PluginSecret `json:"secrets,omitempty"`

	// Config is the configuration section the plugin reads, if it reads one. It
	// is what `hive init` scaffolds, so a new plugin is configurable without a
	// core change.
	Config *PluginConfigManifest `json:"config,omitempty"`
}

// PluginSecret is a credential a plugin needs, named by one or more environment
// variables that may carry it.
//
// An alias is a variable that may carry the same value, so naming both is how a
// plugin says "either of these" rather than "both of these". The core captures
// whichever are set and reports the group only when none is.
type PluginSecret struct {
	// Any lists the environment variables that may carry the value. At least one
	// must be set.
	Any []string `json:"any"`

	Description string `json:"description,omitempty"`
}

// PluginConfigManifest is the configuration a plugin declares.
type PluginConfigManifest struct {
	// Section is the configuration table, for example "transport.zalo".
	Section string `json:"section"`

	// Enabled is the default for the section's enabled flag.
	Enabled bool `json:"enabled,omitempty"`

	// Summary is a one-line description of what the plugin is.
	Summary string `json:"summary,omitempty"`

	// PrincipalHint tells a guided setup how a user finds the id a principal of
	// this transport carries — "@userinfobot reports it", say. A principal is
	// "<transport id>:<user id>" on the security.allowed_users list, and a
	// transport nobody may use does nothing, so the setup asks for it.
	PrincipalHint string `json:"principalHint,omitempty"`

	// Options are the plugin's options, with the values it uses when unset.
	Options []PluginOptionManifest `json:"options,omitempty"`

	// Acknowledgement is the acknowledgement defaults, when the plugin uses one.
	Acknowledgement *PluginAcknowledgementManifest `json:"acknowledgement,omitempty"`
}

// PluginOptionManifest is one configuration option a plugin reads.
type PluginOptionManifest struct {
	Name string `json:"name"`

	// Default is the value the plugin uses when the option is unset. Options are
	// strings on the wire, so it is rendered as one.
	Default string `json:"default,omitempty"`

	Description string `json:"description,omitempty"`

	// Required marks an option that has no working default, so a guided setup
	// asks for it rather than writing the default and leaving a transport that
	// does nothing.
	Required bool `json:"required,omitempty"`
}

// PluginAcknowledgementManifest is the acknowledgement a plugin declares.
//
// Acknowledgement means "received", never "started" or "finished". It is
// presentation data, so what it means is the plugin's business.
type PluginAcknowledgementManifest struct {
	Enabled  bool   `json:"enabled"`
	Mode     string `json:"mode,omitempty"`
	Reaction string `json:"reaction,omitempty"`
}
