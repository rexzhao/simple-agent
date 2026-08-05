package protocol

import "testing"

func TestDecimalHelpers(t *testing.T) {
	for _, value := range []string{"0", "0007", "18446744073709551616"} {
		if err := ValidateDecimal(value); err != nil {
			t.Fatalf("ValidateDecimal(%q) error = %v", value, err)
		}
	}
	for _, value := range []string{"", "-1", "+1", "1.0", " 1"} {
		if err := ValidateDecimal(value); err == nil {
			t.Fatalf("ValidateDecimal(%q) error = nil", value)
		}
	}

	if got, err := CompareDecimal("0007", "7"); err != nil || got != 0 {
		t.Fatalf("CompareDecimal() = %d, %v; want 0, nil", got, err)
	}
	if got, err := CompareDecimal("18446744073709551616", "7"); err != nil || got != 1 {
		t.Fatalf("CompareDecimal() = %d, %v; want 1, nil", got, err)
	}
	if got, err := CompareDecimal("6", "7"); err != nil || got != -1 {
		t.Fatalf("CompareDecimal() = %d, %v; want -1, nil", got, err)
	}

	if got, err := ParseUint64Decimal("18446744073709551615"); err != nil || got != ^uint64(0) {
		t.Fatalf("ParseUint64Decimal(max) = %d, %v", got, err)
	}
	if _, err := ParseUint64Decimal("18446744073709551616"); err == nil {
		t.Fatal("ParseUint64Decimal(overflow) error = nil")
	}

	if err := ValidateResourceRevision(ResourceRevision("revision-718")); err != nil {
		t.Fatalf("ValidateResourceRevision(opaque) error = %v", err)
	}
}
