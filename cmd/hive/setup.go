package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/charmbracelet/huh"

	"github.com/thuupx/hive/internal/config"
	v1 "github.com/thuupx/hive/protocol/hive/v1"
)

// The guided setup.
//
// It exists so a first run does not begin with a file format. Everything it asks
// comes from something that already knows the answer: the agents from the PATH
// scan, the transports and their credentials from the plugins' own manifests, and
// the workspace from the directory the command was run in.
//
// Credentials are never written to the configuration. They go to the secrets file
// the service already uses, which only its owner can read, and `hive serve` loads
// it for anything the environment does not already set.

// initSetupOptions are what the guided setup needs beyond the answers it collects.
type initSetupOptions struct {
	// force allows an existing configuration file to be replaced.
	force bool

	// base is the configuration to start from. Nil means the built-in defaults,
	// which is what `hive init` uses; `hive config --update` passes what is
	// configured now so the answers are prefilled.
	base *config.Config
}

// setupAnswers is what the guided setup collected.
type setupAnswers struct {
	defaultAgent string
	transports   []string
	options      map[string]map[string]string
	secrets      map[string]string
	workspace    string

	// users holds the allowed-user ids asked per transport, as typed: a
	// comma-separated list written to security.allowed_users as
	// "<transport>:<id>" principals.
	users map[string]string

	// extraUsers are the principals carried over unchanged — the ones whose
	// transport is not part of this run and so was never asked about.
	extraUsers []string

	confirmed bool
}

// runSetup walks the user through the configuration and writes it.
func runSetup(cfgPath string, agents []config.DiscoveredAgent, opts initSetupOptions) error {
	manifests := transportManifests(pluginDir())

	dataDir, err := setupDataDir(opts.base)
	if err != nil {
		return err
	}
	stored := readEnvFile(filepath.Join(dataDir, serviceEnvFile))

	answers, err := askSetup(agents, manifests, opts, stored)
	if err != nil {
		return err
	}
	if !answers.confirmed {
		fmt.Println("hive: nothing was written")
		return nil
	}
	return writeSetup(cfgPath, agents, manifests, answers, opts, dataDir)
}

// setupDataDir is where the secrets file lives: the configured data directory
// when there is a configuration to read it from, the default otherwise.
func setupDataDir(base *config.Config) (string, error) {
	if base != nil {
		return base.EffectiveDataDir()
	}
	return config.Default().EffectiveDataDir()
}

// askSetup collects the answers.
func askSetup(agents []config.DiscoveredAgent, manifests []v1.PluginManifest, opts initSetupOptions, stored map[string]string) (setupAnswers, error) {
	answers := setupAnswers{
		options: map[string]map[string]string{},
		secrets: map[string]string{},
		users:   map[string]string{},
	}

	// Prefill from what is configured now, when there is something to prefill.
	if opts.base != nil {
		answers.defaultAgent = opts.base.DefaultAgent
		for name, transport := range opts.base.Transports {
			if transport.Enabled {
				answers.transports = append(answers.transports, name)
			}
		}
		sort.Strings(answers.transports)

		// Allowed users are principals, "<transport>:<id>". The ones a chosen
		// transport owns prefill its field; the rest are carried as given, so
		// a rewrite never drops a principal the form did not ask about.
		for _, principal := range opts.base.Security.AllowedUsers {
			prefix, id, found := strings.Cut(principal, ":")
			if !found || !slices.Contains(answers.transports, prefix) {
				answers.extraUsers = append(answers.extraUsers, principal)
				continue
			}
			answers.users[prefix] = joinIDs(answers.users[prefix], id)
		}
	}
	if answers.defaultAgent == "" && len(agents) > 0 {
		answers.defaultAgent = agents[0].Name
	}

	answers.workspace = initWorkspaceDir()
	if opts.base != nil {
		if dir, err := opts.base.EffectiveWorkspaceDir(); err == nil {
			answers.workspace = dir
		}
	}

	if err := askBasics(&answers, agents, manifests); err != nil {
		return answers, err
	}
	if err := askCredentials(&answers, manifests, stored); err != nil {
		return answers, err
	}
	if err := askConfirmation(&answers, manifests); err != nil {
		return answers, err
	}
	return answers, nil
}

// askBasics asks the agent, transport, and workspace questions.
func askBasics(answers *setupAnswers, agents []config.DiscoveredAgent, manifests []v1.PluginManifest) error {
	var fields []huh.Field

	if len(agents) > 0 {
		fields = append(fields, huh.NewSelect[string]().
			Title("Default agent").
			Description("Used when a session does not choose one. Every agent found on PATH is configured.").
			Options(agentOptions(agents)...).
			Value(&answers.defaultAgent))
	}

	if len(manifests) > 0 {
		fields = append(fields, huh.NewMultiSelect[string]().
			Title("Transports").
			Description("How you talk to Hive. Space selects, enter confirms; none is a valid answer.").
			Options(transportOptions(manifests)...).
			Value(&answers.transports))
	}

	fields = append(fields, huh.NewInput().
		Title("Workspace").
		Description("Where runs work when a session names no location. Leave empty for ~/.hive/workspace.").
		Placeholder(answers.workspace).
		Value(&answers.workspace).
		Validate(validateWorkspace))

	if len(fields) == 0 {
		return nil
	}
	return runForm(huh.NewForm(huh.NewGroup(fields...)))
}

// askCredentials asks each chosen transport for the credentials it declared,
// for the options that have no working default, and for who may use it.
//
// It is a second form because what to ask depends on what was just selected.
// stored is what the secrets file already holds: a credential that exists is
// kept by leaving the field empty rather than being re-typed.
func askCredentials(answers *setupAnswers, manifests []v1.PluginManifest, stored map[string]string) error {
	byID := make(map[string]v1.PluginManifest, len(manifests))
	for _, manifest := range manifests {
		byID[manifest.ID] = manifest
	}

	// A form writes through the pointer it was given, so the answer has to be read
	// back after it runs. Binding each field to a setter is what keeps its storage
	// alive until then — reading a loop variable at construction time reads the
	// empty string it held before the user typed anything.
	type binding struct {
		value *string
		apply func(string)
	}
	var bindings []binding

	var groups []*huh.Group
	for _, id := range answers.transports {
		manifest, ok := byID[id]
		if !ok || manifest.Config == nil {
			continue
		}
		if answers.options[id] == nil {
			answers.options[id] = map[string]string{}
		}

		var fields []huh.Field
		for _, secret := range manifest.Secrets {
			if len(secret.Any) == 0 {
				continue
			}
			// A secret names variables that may carry the same value, so the
			// answer is written under the first of them.
			name := secret.Any[0]
			title := secret.Description
			if title == "" {
				title = strings.Join(secret.Any, " or ")
			}

			value := answers.secrets[name]

			// A credential that already exists — in the secrets file, or in
			// this environment — is kept by answering nothing, so it is not
			// re-typed on every update.
			description := "Stored in a file only you can read; never in the configuration. " + name
			validate := requiredValue(title)
			switch credentialStateOf(secret.Any, stored) {
			case credentialStored:
				description = "Already stored — leave empty to keep it, or enter a new value. " + name
				validate = nil
			case credentialInEnvironment:
				description = "Set in this environment — leave empty to use it, or enter one for the file. " + name
				validate = nil
			}

			field := huh.NewInput().
				Title(title).
				Description(description).
				EchoMode(huh.EchoModePassword).
				Value(&value)
			if validate != nil {
				field = field.Validate(validate)
			}
			bindings = append(bindings, binding{&value, func(v string) { answers.secrets[name] = v }})
			fields = append(fields, field)
		}

		for _, option := range manifest.Config.Options {
			if !option.Required {
				continue
			}
			title := option.Name
			if option.Description != "" {
				title = option.Description
			}

			value := option.Default
			if chosen, ok := answers.options[id][option.Name]; ok {
				value = chosen
			}

			name := option.Name
			bindings = append(bindings, binding{&value, func(v string) { answers.options[id][name] = v }})
			fields = append(fields, huh.NewInput().
				Title(title).
				Value(&value).
				Validate(requiredValue(option.Name)))
		}

		// An enabled transport nobody may use answers no one: unknown access is
		// denied, so the ids go on the security list rather than into a
		// transport section. The transport names the hint; the core writes the
		// principals without knowing what an id looks like.
		ids := answers.users[id]
		description := "User ids the bot answers (comma-separated). Written to security.allowed_users as " + id + ":<id>."
		if hint := manifest.Config.PrincipalHint; hint != "" {
			description += " " + hint
		}
		bindings = append(bindings, binding{&ids, func(v string) { answers.users[id] = v }})
		fields = append(fields, huh.NewInput().
			Title("Allowed users").
			Description(description).
			Value(&ids).
			Validate(requiredValue("an allowed user id")))

		if len(fields) > 0 {
			groups = append(groups, huh.NewGroup(fields...).Title(id))
		}
	}

	if len(groups) == 0 {
		return nil
	}
	if err := runForm(huh.NewForm(groups...)); err != nil {
		return err
	}

	for _, b := range bindings {
		b.apply(*b.value)
	}
	return nil
}

// askConfirmation shows what will be written and asks before writing it.
func askConfirmation(answers *setupAnswers, manifests []v1.PluginManifest) error {
	return runForm(huh.NewForm(huh.NewGroup(
		huh.NewConfirm().
			Title("Write this configuration?").
			Description(setupSummary(answers, manifests)).
			Affirmative("Write it").
			Negative("Cancel").
			Value(&answers.confirmed),
	)))
}

// setupSummary is what the confirmation shows.
//
// It names the credentials rather than showing them: a token on a terminal is a
// token in a scrollback.
func setupSummary(answers *setupAnswers, manifests []v1.PluginManifest) string {
	var b strings.Builder

	fmt.Fprintf(&b, "agent:     %s\n", orNone(answers.defaultAgent))
	fmt.Fprintf(&b, "workspace: %s\n", answers.workspace)

	if len(answers.transports) == 0 {
		b.WriteString("transports: none\n")
	} else {
		fmt.Fprintf(&b, "transports: %s\n", strings.Join(answers.transports, ", "))
	}

	names := make([]string, 0, len(answers.secrets))
	for name := range answers.secrets {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) > 0 {
		fmt.Fprintf(&b, "credentials: %s (written to the secrets file)\n", strings.Join(names, ", "))
	}

	return b.String()
}

// writeSetup writes the configuration and the credentials.
func writeSetup(cfgPath string, agents []config.DiscoveredAgent, manifests []v1.PluginManifest, answers setupAnswers, opts initSetupOptions, dataDir string) error {
	enabled := make([]config.EnabledTransport, 0, len(answers.transports))
	for _, id := range answers.transports {
		for _, manifest := range manifests {
			if manifest.ID == id {
				enabled = append(enabled, config.EnabledTransport{
					Manifest: manifest,
					Options:  answers.options[id],
				})
			}
		}
	}

	content := config.RenderStarter(config.StarterOptions{
		Agents:            agents,
		DefaultAgent:      answers.defaultAgent,
		WorkspaceDir:      answers.workspace,
		Transports:        manifests,
		EnabledTransports: enabled,
		AllowedUsers:      allowedUsers(answers),
		AllowedChannels:   allowedChannels(opts),
	})

	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(cfgPath), err)
	}
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", cfgPath, err)
	}
	fmt.Printf("hive: wrote %s\n", cfgPath)

	if len(answers.secrets) == 0 {
		return nil
	}

	// Merge rather than replace: a credential for a transport that was not part
	// of this run is still needed by the configuration that is already there.
	merged := readEnvFile(filepath.Join(dataDir, serviceEnvFile))
	for name, value := range answers.secrets {
		if value != "" {
			merged[name] = value
		}
	}
	if err := writeServiceEnv(dataDir, merged); err != nil {
		return err
	}

	names := make([]string, 0, len(answers.secrets))
	for name := range answers.secrets {
		names = append(names, name)
	}
	sort.Strings(names)
	fmt.Printf("hive: wrote credentials for %s to %s (%#o)\n",
		strings.Join(names, ", "), filepath.Join(dataDir, serviceEnvFile), serviceEnvMode)
	fmt.Println("hive: the daemon reads them; an exported variable takes precedence")

	printNextSteps()
	return nil
}

// allowedUsers assembles the principal list the security section gets: what
// the form did not ask about first, then each chosen transport's answer, then
// the ids of transports this run did not select — a deselected transport is
// off, but its principals are kept rather than forgotten.
func allowedUsers(answers setupAnswers) []string {
	principals := append([]string{}, answers.extraUsers...)
	seen := map[string]bool{}
	for _, principal := range principals {
		seen[principal] = true
	}
	add := func(more []string) {
		for _, principal := range more {
			if !seen[principal] {
				seen[principal] = true
				principals = append(principals, principal)
			}
		}
	}

	selected := make(map[string]bool, len(answers.transports))
	for _, id := range answers.transports {
		selected[id] = true
		add(principalsFor(id, answers.users[id]))
	}

	rest := make([]string, 0, len(answers.users))
	for id := range answers.users {
		if !selected[id] {
			rest = append(rest, id)
		}
	}
	sort.Strings(rest)
	for _, id := range rest {
		add(principalsFor(id, answers.users[id]))
	}
	return principals
}

// allowedChannels carries the configured channel list through a rewrite. Nil
// for a fresh configuration leaves the default.
func allowedChannels(opts initSetupOptions) []string {
	if opts.base == nil {
		return nil
	}
	return opts.base.Security.AllowedChannels
}

// printNextSteps is what to do now that a configuration exists.
//
// The service is named first, because a daemon in the background is the
// installation that keeps running: one started in a terminal stops when the
// window does.
func printNextSteps() {
	cmd := invokedAs()

	fmt.Println("\nNext:")
	if serviceInstalled() {
		fmt.Printf("  %s service restart\n", cmd)
		fmt.Println("      run the new configuration")
	} else {
		fmt.Printf("  %s service install\n", cmd)
		fmt.Println("      run the daemon in the background, at login")
		fmt.Printf("  %s serve\n", cmd)
		fmt.Println("      ...or run it in this terminal")
	}
	fmt.Printf("  %s doctor\n", cmd)
	fmt.Println("      checks the installation and names what to fix")
}

// invokedAs is how to name this binary in the next steps.
//
// `hive` when that name resolves to this same binary, and this binary's own path
// when it does not. Suggesting a bare `hive` from a build in a checkout would
// install whatever is on PATH instead — a different build, or nothing.
func invokedAs() string {
	self, err := os.Executable()
	if err != nil {
		return "hive"
	}
	found, err := exec.LookPath("hive")
	if err != nil {
		return self
	}
	if resolvePath(found) == resolvePath(self) {
		return "hive"
	}
	return self
}

// agentOptions is the agent list as a form asks for it.
func agentOptions(agents []config.DiscoveredAgent) []huh.Option[string] {
	options := make([]huh.Option[string], 0, len(agents))
	for _, agent := range agents {
		label := fmt.Sprintf("%s  %s", agent.Name, strings.Join(agent.Command, " "))
		if agent.NeedsVerification() {
			label += "  (verify this invocation)"
		}
		options = append(options, huh.NewOption(label, agent.Name))
	}
	return options
}

// transportOptions is the discovered transports as a form asks for them.
func transportOptions(manifests []v1.PluginManifest) []huh.Option[string] {
	options := make([]huh.Option[string], 0, len(manifests))
	for _, manifest := range manifests {
		label := manifest.ID
		if manifest.Config != nil && manifest.Config.Summary != "" {
			label = manifest.ID + "  " + manifest.Config.Summary
		}
		options = append(options, huh.NewOption(label, manifest.ID))
	}
	return options
}

// validateWorkspace refuses a directory that is not a project.
//
// The same rule the configuration enforces, said while the answer is being typed
// rather than after the file is written.
func validateWorkspace(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if !filepath.IsAbs(value) {
		return errors.New("use an absolute path")
	}
	cleaned := filepath.Clean(value)
	if cleaned == string(filepath.Separator) {
		return errors.New("/ is the whole filesystem, not a workspace")
	}
	if home, err := os.UserHomeDir(); err == nil && cleaned == home {
		return errors.New("the home directory is not a project")
	}
	return nil
}

// credentialState is where a secret already lives, if anywhere.
type credentialState int

const (
	credentialMissing credentialState = iota
	// credentialStored is already in the secrets file.
	credentialStored
	// credentialInEnvironment is exported in this shell. It does not reach a
	// service on its own, but it serves a terminal run and a service install
	// collects it, so it counts as present.
	credentialInEnvironment
)

// credentialStateOf reports whether one of a secret's variables already has a
// value: in the secrets file first, then in this environment.
func credentialStateOf(names []string, stored map[string]string) credentialState {
	for _, name := range names {
		if stored[name] != "" {
			return credentialStored
		}
	}
	for _, name := range names {
		if os.Getenv(name) != "" {
			return credentialInEnvironment
		}
	}
	return credentialMissing
}

// joinIDs appends an id to a comma-separated list.
func joinIDs(list, id string) string {
	if list == "" {
		return id
	}
	return list + ", " + id
}

// splitIDs reads a comma- or space-separated list of user ids.
func splitIDs(raw string) []string {
	return strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' })
}

// principalsFor turns one transport's id answer into principals. An id
// already written as a principal — "<transport>:<id>", pasted whole — is kept
// as given; a bare id gets its transport's prefix.
func principalsFor(transport, raw string) []string {
	var principals []string
	for _, id := range splitIDs(raw) {
		if strings.Contains(id, ":") {
			principals = append(principals, id)
		} else {
			principals = append(principals, transport+":"+id)
		}
	}
	return principals
}

// requiredValue refuses an empty answer.
func requiredValue(what string) func(string) error {
	return func(value string) error {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", what)
		}
		return nil
	}
}

// runForm runs a form, treating an abort as "ask nothing further".
func runForm(form *huh.Form) error {
	err := form.Run()
	if errors.Is(err, huh.ErrUserAborted) {
		return nil
	}
	return err
}

// orNone renders an empty answer.
func orNone(value string) string {
	if value == "" {
		return "none"
	}
	return value
}

// runConfigUpdate changes an existing configuration with the guided setup.
func runConfigUpdate(f flags) error {
	if !isInteractive() {
		return errors.New("changing the configuration asks questions, so it needs a terminal; edit the file instead")
	}

	cfgPath := f.configPath
	if cfgPath == "" {
		var err error
		if cfgPath, err = config.DefaultPath(); err != nil {
			return err
		}
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	applyOverrides(&cfg, f)

	return runSetup(cfgPath, config.DiscoverAgents(), initSetupOptions{force: true, base: &cfg})
}
