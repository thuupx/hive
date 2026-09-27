package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	confirmed    bool
}

// runSetup walks the user through the configuration and writes it.
func runSetup(cfgPath string, agents []config.DiscoveredAgent, opts initSetupOptions) error {
	manifests := transportManifests(pluginDir())

	answers, err := askSetup(agents, manifests, opts)
	if err != nil {
		return err
	}
	if !answers.confirmed {
		fmt.Println("hive: nothing was written")
		return nil
	}
	return writeSetup(cfgPath, agents, manifests, answers)
}

// askSetup collects the answers.
func askSetup(agents []config.DiscoveredAgent, manifests []v1.PluginManifest, opts initSetupOptions) (setupAnswers, error) {
	answers := setupAnswers{
		options: map[string]map[string]string{},
		secrets: map[string]string{},
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
	if err := askCredentials(&answers, manifests); err != nil {
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

// askCredentials asks each chosen transport for the credentials it declared, and
// for the options that have no working default.
//
// It is a second form because what to ask depends on what was just selected.
func askCredentials(answers *setupAnswers, manifests []v1.PluginManifest) error {
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
			bindings = append(bindings, binding{&value, func(v string) { answers.secrets[name] = v }})
			fields = append(fields, huh.NewInput().
				Title(title).
				Description("Stored in a file only you can read; never in the configuration. "+name).
				EchoMode(huh.EchoModePassword).
				Value(&value).
				Validate(requiredValue(title)))
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
func writeSetup(cfgPath string, agents []config.DiscoveredAgent, manifests []v1.PluginManifest, answers setupAnswers) error {
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

	dir, err := config.Default().EffectiveDataDir()
	if err != nil {
		return err
	}
	// Merge rather than replace: a credential for a transport that was not part
	// of this run is still needed by the configuration that is already there.
	merged := readEnvFile(filepath.Join(dir, serviceEnvFile))
	for name, value := range answers.secrets {
		if value != "" {
			merged[name] = value
		}
	}
	if err := writeServiceEnv(dir, merged); err != nil {
		return err
	}

	names := make([]string, 0, len(answers.secrets))
	for name := range answers.secrets {
		names = append(names, name)
	}
	sort.Strings(names)
	fmt.Printf("hive: wrote credentials for %s to %s (%#o)\n",
		strings.Join(names, ", "), filepath.Join(dir, serviceEnvFile), serviceEnvMode)
	fmt.Println("hive: the daemon reads them; an exported variable takes precedence")

	printNextSteps()
	return nil
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
