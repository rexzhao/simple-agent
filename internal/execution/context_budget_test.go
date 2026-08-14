package execution

import (
	"strconv"
	"testing"
)

func TestResolveEffectiveContextBudgetAcrossWindowSizes(t *testing.T) {
	for _, window := range []int{32_000, 64_000, 128_000, 256_000, 512_000, 1_000_000} {
		t.Run(strconv.Itoa(window), func(t *testing.T) {
			budget := resolveEffectiveContextBudget(window, 0, window/8, 0, 80)
			if budget.ReservedOutput != window/8 {
				t.Fatalf("ReservedOutput = %d, want %d", budget.ReservedOutput, window/8)
			}
			if !(budget.TargetTokens < budget.SoftInputLimit && budget.SoftInputLimit < budget.HardInputLimit && budget.HardInputLimit < window-budget.ReservedOutput+1) {
				t.Fatalf("invalid ordered budget: %#v", budget)
			}
			if budget.SafetyMargin < contextSafetyMin || budget.SafetyMargin > contextSafetyMax {
				t.Fatalf("SafetyMargin = %d outside bounds", budget.SafetyMargin)
			}
		})
	}
}

func TestResolveEffectiveContextBudgetDoesNotSubtractOutputFromIndependentInputLimit(t *testing.T) {
	budget := resolveEffectiveContextBudget(400_000, 272_000, 128_000, 0, 80)
	if budget.HardInputLimit != 272_000-budget.SafetyMargin {
		t.Fatalf("HardInputLimit = %d, want independent input limit %d minus safety %d", budget.HardInputLimit, 272_000, budget.SafetyMargin)
	}
}

func TestResolveEffectiveContextBudgetBoundsUnknownOutputFallback(t *testing.T) {
	if got := resolveEffectiveContextBudget(32_000, 0, 0, 0, 80).ReservedOutput; got != 4*1024 {
		t.Fatalf("32K fallback reserve = %d, want 4096", got)
	}
	if got := resolveEffectiveContextBudget(1_000_000, 0, 0, 0, 80).ReservedOutput; got != 20*1024 {
		t.Fatalf("1M fallback reserve = %d, want 20480", got)
	}
}
