package sdk

import (
	"encoding/json"
	"os"
	"testing"

	v1 "github.com/thuupx/hive/protocol/hive/v1"
)

// Describe writes a manifest the core can read back.
func TestDescribeWritesAManifest(t *testing.T) {
	manifest := v1.PluginManifest{
		ID:      "demo",
		Type:    v1.PluginTypeTransport,
		Version: "1.2.3",
		Secrets: []v1.PluginSecret{{Any: []string{"DEMO_TOKEN"}}},
		Config: &v1.PluginConfigManifest{
			Section: "transport.demo",
			Options: []v1.PluginOptionManifest{{Name: "greeting", Default: "hi"}},
		},
	}

	read, write, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	old := os.Stdout
	os.Stdout = write

	if err := Describe(manifest); err != nil {
		os.Stdout = old
		t.Fatalf("describe: %v", err)
	}
	write.Close()
	os.Stdout = old

	var decoded v1.PluginManifest
	if err := json.NewDecoder(read).Decode(&decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.ID != "demo" || decoded.Type != v1.PluginTypeTransport || decoded.Version != "1.2.3" {
		t.Fatalf("decoded = %+v", decoded)
	}
	if len(decoded.Secrets) != 1 || len(decoded.Secrets[0].Any) != 1 || decoded.Secrets[0].Any[0] != "DEMO_TOKEN" {
		t.Fatalf("secrets = %v", decoded.Secrets)
	}
	if decoded.Config == nil || decoded.Config.Section != "transport.demo" {
		t.Fatalf("config = %+v", decoded.Config)
	}
}

// An option the manifest does not declare is reported, so a misspelling does not
// silently do nothing. The acknowledgement keys every transport receives are not
// unknown.
func TestUnknownOptions(t *testing.T) {
	manifest := v1.PluginManifest{
		Config: &v1.PluginConfigManifest{
			Section: "transport.demo",
			Options: []v1.PluginOptionManifest{{Name: "greeting"}},
		},
	}
	provided := map[string]string{
		"greeting":                 "hi",
		"acknowledgement":          "true",
		"acknowledgement_mode":     "visual",
		"acknowledgement_reaction": "eyes",
		"bot_user_id":              "typo",
	}

	got := UnknownOptions(manifest, provided)
	if len(got) != 1 || got[0] != "bot_user_id" {
		t.Fatalf("unknown = %v, want only bot_user_id", got)
	}
}
