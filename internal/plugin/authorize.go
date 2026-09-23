// Package plugin supervises plugins and enforces the plugin boundary.
//
// Plugins always run as separate processes. Process isolation provides fault
// containment, not a security sandbox: a trusted plugin has powerful access to
// Hive through its plugin protocol, and the public documentation must say so
// rather than imply the first version already sandboxes plugins.
package plugin

import (
	"sort"

	v1 "github.com/thupham/hive/protocol/hive/v1"
)

// methodCapability maps a plugin-invokable method to the capability it needs.
var methodCapability = map[string]string{
	v1.MethodSessionCreate:     v1.CapabilitySessionWrite,
	v1.MethodSessionPrompt:     v1.CapabilitySessionWrite,
	v1.MethodSessionCancel:     v1.CapabilitySessionWrite,
	v1.MethodSessionHandoff:    v1.CapabilitySessionWrite,
	v1.MethodSessionList:       v1.CapabilitySessionRead,
	v1.MethodSessionStatus:     v1.CapabilitySessionRead,
	v1.MethodAgentList:         v1.CapabilityAgentRead,
	v1.MethodEventSubscribe:    v1.CapabilityEventRead,
	v1.MethodEventAck:          v1.CapabilityEventRead,
	v1.MethodEventPublish:      v1.CapabilityEventWrite,
	v1.MethodPermissionList:    v1.CapabilityPermissionRead,
	v1.MethodPermissionRespond: v1.CapabilityPermissionWrite,
	v1.MethodPermissionRequest: v1.CapabilityPermissionWrite,
	v1.MethodExecutionStart:    v1.CapabilityExecutionWrite,
	v1.MethodExecutionPrompt:   v1.CapabilityExecutionWrite,
	v1.MethodExecutionCancel:   v1.CapabilityExecutionWrite,
	v1.MethodExecutionReport:   v1.CapabilityExecutionWrite,
	v1.MethodTransportInbound:  v1.CapabilityTransportInbound,
}

// grantable is what each plugin type may ever be granted.
//
// A plugin cannot widen its authority by asking: the core intersects the
// requested set with this table, so registration is descriptive and never a
// grant.
var grantable = map[v1.PluginType]map[string]bool{
	v1.PluginTypeTransport: {
		v1.CapabilitySessionRead:      true,
		v1.CapabilitySessionWrite:     true,
		v1.CapabilityAgentRead:        true,
		v1.CapabilityEventRead:        true,
		v1.CapabilityPermissionRead:   true,
		v1.CapabilityPermissionWrite:  true,
		v1.CapabilityTransportInbound: true,
	},
	v1.PluginTypeAgent: {
		v1.CapabilityEventWrite:      true,
		v1.CapabilityPermissionWrite: true,
		v1.CapabilityExecutionWrite:  true,
	},
	v1.PluginTypeUI: {
		v1.CapabilitySessionRead:    true,
		v1.CapabilityAgentRead:      true,
		v1.CapabilityEventRead:      true,
		v1.CapabilityPermissionRead: true,
	},
	v1.PluginTypeNetwork: {},
}

// Grant returns the capabilities a plugin of this type may actually hold.
//
// Unknown capabilities are dropped rather than rejected, so a plugin built
// against a newer protocol can still connect with the subset this core
// understands.
func Grant(t v1.PluginType, requested []string) []string {
	allowed := grantable[t]

	var out []string
	seen := map[string]bool{}
	for _, capability := range requested {
		if !allowed[capability] || seen[capability] {
			continue
		}
		seen[capability] = true
		out = append(out, capability)
	}
	sort.Strings(out)
	return out
}

// AuthorizeMethod reports whether granted capabilities permit method.
func AuthorizeMethod(granted []string, method string) error {
	required, known := methodCapability[method]
	if !known {
		return v1.Unauthorized("method is not part of the plugin API: %s", method)
	}
	for _, capability := range granted {
		if capability == required {
			return nil
		}
	}
	return v1.Unauthorized("not authorized for %s (requires %s)", method, required)
}
