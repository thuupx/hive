// Package control serves the Hive Control API.
//
// The Control API is transport-independent. Authentication is
// transport-specific; authorization is a Hive domain concern and is evaluated
// before a mutating operation is accepted.
package control

import (
	"strings"

	v1 "github.com/thuupx/hive/protocol/hive/v1"
)

// Principal is an authenticated actor.
type Principal string

// OwnerPrincipal is the local owner of a personal installation.
const OwnerPrincipal Principal = "owner"

// Policy is the authorization policy.
//
// Unknown access is denied by default: a principal that is neither the owner, nor
// explicitly allowed, nor a trusted plugin cannot act.
type Policy struct {
	// Owner is the principal that owns this installation.
	Owner Principal

	// AllowedUsers are additional *user* principals permitted to act. A transport
	// asserts these, so each one has to be named explicitly.
	AllowedUsers map[string]bool

	// Plugins are trusted plugin identities.
	//
	// A plugin is an authenticated local process whose capabilities the core
	// already granted, so it may act as itself. It is not a user: it still cannot
	// assert a user principal that is not allowed.
	Plugins map[string]bool
}

// NewPolicy builds a policy from the configured allow list and plugin identities.
func NewPolicy(allowedUsers, plugins []string) Policy {
	allowed := make(map[string]bool, len(allowedUsers))
	for _, user := range allowedUsers {
		allowed[user] = true
	}

	trusted := make(map[string]bool, len(plugins))
	for _, plugin := range plugins {
		trusted[plugin] = true
	}

	return Policy{Owner: OwnerPrincipal, AllowedUsers: allowed, Plugins: trusted}
}

// Authorize reports whether principal may invoke a method.
func (p Policy) Authorize(principal Principal, method string) error {
	switch {
	case principal == "":
		return v1.Unauthorized("no authenticated principal")
	case principal == p.Owner:
		return nil
	case p.AllowedUsers[string(principal)]:
		return nil
	case p.Plugins[string(principal)]:
		return nil
	default:
		return v1.Unauthorized("principal %s is not allowed to invoke %s", principal, method)
	}
}

// TransportAssertion is a principal asserted by a trusted transport plugin.
//
// A transport plugin is trusted for its own transport, so it may assert a
// principal for the user it authenticated — but only one that belongs to that
// transport. A plugin cannot assert an arbitrary principal, because an
// unverified, client-supplied principal string is never proof of identity.
type TransportAssertion struct {
	Transport string
	Principal Principal
}

// Validate reports whether the asserted principal belongs to the transport.
func (a TransportAssertion) Validate() error {
	switch {
	case a.Transport == "":
		return v1.Unauthorized("a transport name is required to assert a principal")
	case a.Principal == "":
		return v1.Unauthorized("an asserted principal is required")
	case !strings.HasPrefix(string(a.Principal), a.Transport+":"):
		return v1.Unauthorized("principal %s does not belong to transport %s", a.Principal, a.Transport)
	}
	return nil
}

// Connection describes who is on the other end of a Control API link.
type Connection struct {
	// Principal is the authenticated identity of the connection.
	Principal Principal

	// Transport, when set, marks the connection as a trusted transport for that
	// transport name.
	Transport string
}

// ActingPrincipal resolves which principal a message acts as.
//
// A plain client acts as itself. A trusted transport may assert a principal per
// message, but the assertion is validated against the transport, so the string
// alone never grants authority.
func (c Connection) ActingPrincipal(actor string) (Principal, error) {
	claimed := Principal(actor)
	if claimed == "" || c.Transport == "" {
		return c.Principal, nil
	}
	if err := (TransportAssertion{Transport: c.Transport, Principal: claimed}).Validate(); err != nil {
		return "", err
	}
	return claimed, nil
}
