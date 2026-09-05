package money

import (
	"encoding/json"
	"fmt"
)

// MarshalJSON encodes the Amount as a JSON string, never as a number.
func (a Amount) MarshalJSON() ([]byte, error) {
	return json.Marshal(a.String())
}

// UnmarshalJSON accepts only a JSON string containing a canonical decimal.
// A JSON number is rejected so that clients cannot lose precision silently.
func (a *Amount) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("%w: amounts must be JSON strings: %w", ErrInvalidAmount, err)
	}
	parsed, err := ParseAmount(s)
	if err != nil {
		return err
	}
	*a = parsed
	return nil
}

// MarshalText implements encoding.TextMarshaler (query params, map keys).
func (a Amount) MarshalText() ([]byte, error) { return []byte(a.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (a *Amount) UnmarshalText(b []byte) error {
	parsed, err := ParseAmount(string(b))
	if err != nil {
		return err
	}
	*a = parsed
	return nil
}
