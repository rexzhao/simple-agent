package execution

import "github.com/rexzhao/simple-agent/internal/contextwindow"

const (
	contextSafetyPercent = 2
	contextSafetyMin     = 512
	contextSafetyMax     = 8 * 1024
	softHeadroomMin      = 2 * 1024
	softHeadroomMax      = 32 * 1024
)

type effectiveContextBudget struct {
	ContextWindow  int
	MaxInputTokens int
	ReservedOutput int
	SafetyMargin   int
	SoftInputLimit int
	HardInputLimit int
	TargetTokens   int
}

func resolveEffectiveContextBudget(contextWindow, maxInputTokens, maxOutputTokens, configuredReserve, thresholdPercent int) effectiveContextBudget {
	reservedOutput := maxOutputTokens
	if reservedOutput <= 0 {
		reservedOutput = configuredReserve
	}
	if reservedOutput <= 0 && contextWindow > 0 {
		reservedOutput = clampInt(contextWindow/8, 4*1024, 20*1024)
	}

	capacity := 0
	if contextWindow > 0 {
		capacity = contextWindow - reservedOutput
	}
	if maxInputTokens > 0 && (capacity <= 0 || maxInputTokens < capacity) {
		capacity = maxInputTokens
	}
	if capacity <= 0 {
		return effectiveContextBudget{ContextWindow: contextWindow, MaxInputTokens: maxInputTokens, ReservedOutput: reservedOutput}
	}

	safety := clampInt((capacity*contextSafetyPercent+99)/100, contextSafetyMin, contextSafetyMax)
	hard := capacity - safety
	if hard <= 0 {
		return effectiveContextBudget{ContextWindow: contextWindow, MaxInputTokens: maxInputTokens, ReservedOutput: reservedOutput, SafetyMargin: safety}
	}
	if thresholdPercent <= 0 {
		thresholdPercent = contextwindow.WarningThresholdPercent
	}
	if thresholdPercent > 100 {
		thresholdPercent = 100
	}
	softHeadroom := clampInt((hard*(100-thresholdPercent)+99)/100, softHeadroomMin, softHeadroomMax)
	soft := hard - softHeadroom
	if soft <= 0 {
		soft = hard
	}
	target := soft - softHeadroom
	if target <= 0 {
		target = maxInt(1, soft/2)
	}
	return effectiveContextBudget{
		ContextWindow: contextWindow, MaxInputTokens: maxInputTokens,
		ReservedOutput: reservedOutput, SafetyMargin: safety,
		SoftInputLimit: soft, HardInputLimit: hard, TargetTokens: target,
	}
}

func clampInt(value, minimum, maximum int) int {
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
