package protocol

import (
	"fmt"
	"math/big"
	"strings"
)

type Sequence string

type ResourceRevision string

type RunCursor string

// ValidateDecimal accepts a non-negative decimal wire token. Leading zeroes
// are accepted so validation remains compatible with producers that serialize
// an integer token without canonicalizing it.
func ValidateDecimal(value string) error {
	if value == "" {
		return fmt.Errorf("must be a non-negative decimal string")
	}
	for index := 0; index < len(value); index++ {
		if value[index] < '0' || value[index] > '9' {
			return fmt.Errorf("must be a non-negative decimal string")
		}
	}
	return nil
}

// IsNonNegativeDecimal reports whether value is a valid decimal wire token.
func IsNonNegativeDecimal(value string) bool {
	return ValidateDecimal(value) == nil
}

// ParseDecimal parses an arbitrarily large non-negative decimal without going
// through a machine-sized integer.
func ParseDecimal(value string) (*big.Int, error) {
	if err := ValidateDecimal(value); err != nil {
		return nil, err
	}
	parsed, ok := new(big.Int).SetString(value, 10)
	if !ok {
		return nil, fmt.Errorf("invalid decimal string")
	}
	return parsed, nil
}

// ParseUint64Decimal is the bounded form for protocol consumers that require a
// uint64. It rejects overflow instead of wrapping.
func ParseUint64Decimal(value string) (uint64, error) {
	parsed, err := ParseDecimal(value)
	if err != nil {
		return 0, err
	}
	if !parsed.IsUint64() {
		return 0, fmt.Errorf("decimal value overflows uint64")
	}
	return parsed.Uint64(), nil
}

// CompareDecimal compares two non-negative decimal tokens without converting
// them to float64 or an overflowing machine integer.
func CompareDecimal(left, right string) (int, error) {
	if err := ValidateDecimal(left); err != nil {
		return 0, fmt.Errorf("left value: %w", err)
	}
	if err := ValidateDecimal(right); err != nil {
		return 0, fmt.Errorf("right value: %w", err)
	}
	left = normalizeDecimal(left)
	right = normalizeDecimal(right)
	if len(left) < len(right) {
		return -1, nil
	}
	if len(left) > len(right) {
		return 1, nil
	}
	return strings.Compare(left, right), nil
}

func normalizeDecimal(value string) string {
	trimmed := strings.TrimLeft(value, "0")
	if trimmed == "" {
		return "0"
	}
	return trimmed
}

func ValidateSequence(value Sequence) error {
	return ValidateDecimal(string(value))
}

func ValidateResourceRevision(value ResourceRevision) error {
	if strings.TrimSpace(string(value)) == "" {
		return fmt.Errorf("must be a non-empty resource revision string")
	}
	return nil
}

func ValidateRunCursor(value RunCursor) error {
	if err := ValidateDecimal(string(value)); err != nil {
		return err
	}
	if len(value) > 1 && value[0] == '0' {
		return fmt.Errorf("must be a canonical non-negative decimal string")
	}
	return nil
}

func CompareSequence(left, right Sequence) (int, error) {
	return CompareDecimal(string(left), string(right))
}

func CompareRunCursor(left, right RunCursor) (int, error) {
	if err := ValidateRunCursor(left); err != nil {
		return 0, fmt.Errorf("left value: %w", err)
	}
	if err := ValidateRunCursor(right); err != nil {
		return 0, fmt.Errorf("right value: %w", err)
	}
	return CompareDecimal(string(left), string(right))
}

func ParseSequence(value Sequence) (*big.Int, error) {
	return ParseDecimal(string(value))
}

func ParseRunCursor(value RunCursor) (*big.Int, error) {
	if err := ValidateRunCursor(value); err != nil {
		return nil, err
	}
	return ParseDecimal(string(value))
}
