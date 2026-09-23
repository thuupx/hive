package control

import (
	"errors"
	"testing"

	v1 "github.com/thupham/hive/protocol/hive/v1"
)

func TestPolicyAllowsOwnerAndConfiguredUsers(t *testing.T) {
	policy := NewPolicy([]string{"slack:U123"})

	if err := policy.Authorize(OwnerPrincipal, v1.MethodSessionCreate); err != nil {
		t.Errorf("the owner must be allowed: %v", err)
	}
	if err := policy.Authorize("slack:U123", v1.MethodSessionPrompt); err != nil {
		t.Errorf("a configured user must be allowed: %v", err)
	}
}

// Unknown access is denied by default.
func TestPolicyDeniesUnknownAndEmptyPrincipals(t *testing.T) {
	policy := NewPolicy([]string{"slack:U123"})

	for _, principal := range []Principal{"", "telegram:1", "slack:U999"} {
		if err := policy.Authorize(principal, v1.MethodSessionCreate); err == nil {
			t.Errorf("principal %q must be denied", principal)
		}
	}
}

func TestTransportAssertionMustBelongToTheTransport(t *testing.T) {
	ok := TransportAssertion{Transport: "slack", Principal: "slack:U123"}
	if err := ok.Validate(); err != nil {
		t.Fatalf("a matching assertion must be accepted: %v", err)
	}

	cases := map[string]TransportAssertion{
		"no transport":    {Principal: "slack:U123"},
		"no principal":    {Transport: "slack"},
		"other transport": {Transport: "slack", Principal: "telegram:1"},
		"unprefixed":      {Transport: "slack", Principal: "U123"},
	}
	for name, assertion := range cases {
		t.Run(name, func(t *testing.T) {
			if err := assertion.Validate(); err == nil {
				t.Fatal("expected the assertion to be rejected")
			}
		})
	}
}

// A plain client acts as itself and cannot escalate by naming a principal.
func TestConnectionIgnoresActorForAPlainClient(t *testing.T) {
	conn := Connection{Principal: OwnerPrincipal}

	got, err := conn.ActingPrincipal("slack:U123")
	if err != nil {
		t.Fatalf("ActingPrincipal: %v", err)
	}
	if got != OwnerPrincipal {
		t.Fatalf("principal = %q, want the connection principal", got)
	}
}

// A trusted transport may assert a principal, but only one of its own.
func TestConnectionAcceptsATransportAssertion(t *testing.T) {
	conn := Connection{Principal: "plugin:slack", Transport: "slack"}

	got, err := conn.ActingPrincipal("slack:U123")
	if err != nil {
		t.Fatalf("ActingPrincipal: %v", err)
	}
	if got != "slack:U123" {
		t.Fatalf("principal = %q, want the asserted principal", got)
	}

	if _, err := conn.ActingPrincipal("telegram:1"); err == nil {
		t.Fatal("a transport must not assert a principal from another transport")
	}
}

// A transport that asserts nothing falls back to its own identity.
func TestConnectionFallsBackWhenNothingIsAsserted(t *testing.T) {
	conn := Connection{Principal: "plugin:slack", Transport: "slack"}

	got, err := conn.ActingPrincipal("")
	if err != nil {
		t.Fatalf("ActingPrincipal: %v", err)
	}
	if got != "plugin:slack" {
		t.Fatalf("principal = %q, want the connection principal", got)
	}
}

func TestAuthorizationErrorsAreProtocolErrors(t *testing.T) {
	policy := NewPolicy(nil)

	err := policy.Authorize("stranger", v1.MethodSessionCreate)
	if err == nil {
		t.Fatal("expected denial")
	}

	var protoErr *v1.Error
	if !errors.As(err, &protoErr) {
		t.Fatalf("err = %T, want a protocol error", err)
	}
	if protoErr.Code != v1.CodeUnauthorized {
		t.Fatalf("code = %d, want %d", protoErr.Code, v1.CodeUnauthorized)
	}
}
