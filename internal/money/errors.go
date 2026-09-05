package money

import "errors"

var (
	// ErrInvalidAmount is returned when a string is not a plain decimal
	// literal (optional leading minus, digits, optional fraction).
	ErrInvalidAmount = errors.New("money: invalid amount")
	// ErrPrecision is returned when a value does not fit NUMERIC(36,18).
	ErrPrecision = errors.New("money: precision exceeds NUMERIC(36,18)")
	// ErrInvalidStep is returned when a tick/step used for IsMultipleOf is
	// not strictly positive.
	ErrInvalidStep = errors.New("money: step must be positive")
	// ErrInvalidAsset is returned by Asset.Validate.
	ErrInvalidAsset = errors.New("money: invalid asset")
)
