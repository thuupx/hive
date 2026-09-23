package v1

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// ID is a JSON-RPC request identifier. JSON-RPC allows a string or a
// number. Hive preserves the exact type so a response echoes the request
// identifier unchanged.
//
// The zero ID is unset and marshals as null. A request without an ID is a
// notification.
type ID struct {
	value string
	isStr bool
	valid bool
}

// StringID returns a string identifier.
func StringID(s string) ID { return ID{value: s, isStr: true, valid: true} }

// NumberID returns a numeric identifier.
func NumberID(n uint64) ID { return ID{value: strconv.FormatUint(n, 10), valid: true} }

// Valid reports whether the identifier is set.
func (i ID) Valid() bool { return i.valid }

// IsString reports whether the identifier was a JSON string.
func (i ID) IsString() bool { return i.isStr }

// String returns the identifier as text, or "" when unset.
func (i ID) String() string {
	if !i.valid {
		return ""
	}
	return i.value
}

// Number returns the numeric value when the identifier is numeric.
func (i ID) Number() (uint64, bool) {
	if !i.valid || i.isStr {
		return 0, false
	}
	n, err := strconv.ParseUint(i.value, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func (i ID) MarshalJSON() ([]byte, error) {
	if !i.valid {
		return []byte("null"), nil
	}
	if i.isStr {
		return json.Marshal(i.value)
	}
	return []byte(i.value), nil
}

func (i *ID) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*i = ID{}
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*i = StringID(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	if _, err := strconv.ParseUint(n.String(), 10, 64); err != nil {
		return fmt.Errorf("hive: invalid numeric id %q", n.String())
	}
	*i = ID{value: n.String(), valid: true}
	return nil
}
