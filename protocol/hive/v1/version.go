// Package v1 defines the Hive Protocol version 1 wire types.
//
// This package is the public protocol surface. It must not import any
// internal Hive package, and it depends on the Go standard library only.
// Plugins and external clients depend on this package without linking the
// Hive core. See docs/adr for the plugin boundary decision.
package v1

import "fmt"

// ProtocolFamily identifies the Hive Protocol family.
const ProtocolFamily = "hive"

// ProtocolMajor is the Hive Protocol major version. A major bump is a
// breaking change.
const ProtocolMajor = 1

// ProtocolMinor is the Hive Protocol minor version. Minor bumps are
// additive only.
const ProtocolMinor = 0

// JSONRPCVersion is the only JSON-RPC version supported by Hive Protocol.
const JSONRPCVersion = "2.0"

// ProtocolVersion identifies a Hive Protocol version.
//
// Hive distinguishes the protocol family/version from individual message
// or event versions. Message and event versions are carried separately by
// the domain types that need them.
type ProtocolVersion struct {
	Family string `json:"family"`
	Major  int    `json:"major"`
	Minor  int    `json:"minor"`
}

// Current returns the protocol version implemented by this build.
func Current() ProtocolVersion {
	return ProtocolVersion{Family: ProtocolFamily, Major: ProtocolMajor, Minor: ProtocolMinor}
}

// Compatible reports whether this implementation can serve v.
//
// Compatibility requires the same family and the same major version. A
// lower minor version is compatible because minor bumps are additive. A
// higher minor version is not, because it may rely on fields this build
// does not know about.
func (v ProtocolVersion) Compatible() bool {
	return v.Family == ProtocolFamily && v.Major == ProtocolMajor && v.Minor <= ProtocolMinor
}

func (v ProtocolVersion) String() string {
	return fmt.Sprintf("%s/%d.%d", v.Family, v.Major, v.Minor)
}
