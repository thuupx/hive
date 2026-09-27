package sdk

import (
	"encoding/json"
	"fmt"
	"os"

	v1 "github.com/thuupx/hive/protocol/hive/v1"
)

// DescribeFlag is the flag the core passes to ask a plugin what it needs.
//
// It is a process flag rather than a protocol method on purpose: the core asks a
// plugin about itself without starting it, so the answer cannot depend on a
// connection that only exists once the plugin has started.
const DescribeFlag = "describe"

// Describe writes a plugin's manifest to standard output.
//
// A plugin calls this when it was started with -describe, before it connects, and
// then exits. The core reads the manifest to know which environment variables a
// service must carry and which configuration section to scaffold.
func Describe(manifest v1.PluginManifest) error {
	encoder := json.NewEncoder(os.Stdout)
	// A manifest is read by a person as often as by the core, so it is not
	// HTML-escaped: "<bot id>:<secret>" should read as itself.
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")

	if err := encoder.Encode(manifest); err != nil {
		return fmt.Errorf("sdk: write manifest: %w", err)
	}
	return nil
}
