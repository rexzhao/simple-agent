package agent

import (
	"encoding/json"
	"math"
	"unicode/utf8"

	"github.com/rexzhao/simple-agent/model"
)

// Token estimation and the context budget, ported from the previous
// stack's contextwindow/execution layers. The estimate is a proven
// heuristic: ASCII characters count as one token per four, non-ASCII
// (CJK in particular) as one token per character, plus per-message and
// per-request overheads.

const (
	requestOverheadTokens = 16
	messageOverheadTokens = 8
	estimatedImageTokens  = 1_000

	// Budget derivation bounds (matching the previous stack).
	contextSafetyPercent = 2
	contextSafetyMin     = 512
	contextSafetyMax     = 8 * 1024
	softHeadroomPercent  = 20 // soft limit sits 20% of the hard limit below it
	softHeadroomMin      = 2 * 1024
	softHeadroomMax      = 32 * 1024
	reservedOutputMin    = 4 * 1024
	reservedOutputMax    = 20 * 1024

	// DefaultContextWindowTokens is the conservative window assumed when a
	// model profile does not configure one.
	DefaultContextWindowTokens = 32_000
)

// estimateRequestTokens estimates the input size of a request.
func estimateRequestTokens(request model.Request) int {
	total := requestOverheadTokens
	for _, message := range request.Messages {
		total += estimateMessageTokens(message)
	}
	for _, tool := range request.Tools {
		total += estimateTextTokens(tool.Name) + estimateTextTokens(tool.Description)
		if encoded, err := json.Marshal(tool.InputSchema); err == nil {
			total += estimateTextTokens(string(encoded))
		}
	}
	if total < 1 {
		return 1
	}
	return total
}

func estimateMessageTokens(message model.Message) int {
	total := messageOverheadTokens
	total += estimateTextTokens(string(message.Role))
	total += estimateTextTokens(message.Content)
	total += estimateTextTokens(message.ReasoningContent)
	total += estimateTextTokens(message.ToolCallID)
	for _, call := range message.ToolCalls {
		total += estimateTextTokens(call.ID) + estimateTextTokens(call.Name) + estimateTextTokens(call.Arguments)
	}
	for _, block := range message.ContentBlocks {
		total += estimateTextTokens(block.Type) + estimateTextTokens(block.Text)
		if block.Type == "input_image" {
			total += estimatedImageTokens
		} else {
			total += estimateTextTokens(block.ImageURL)
		}
	}
	return total
}

func estimateTextTokens(text string) int {
	if text == "" {
		return 0
	}
	ascii, nonASCII := 0, 0
	for _, character := range text {
		if character < utf8.RuneSelf {
			ascii++
		} else {
			nonASCII++
		}
	}
	return int(math.Ceil(float64(ascii)/4)) + nonASCII
}

// contextBudget holds the compaction pressure limits of one model.
type contextBudget struct {
	SoftInputLimit int // at or above: compact when the guards allow
	HardInputLimit int // at or above: compact, and fail if it does not help
}

// resolveContextBudget derives the pressure limits from the model's
// context window and output limit: capacity is the window minus the
// reserved output; the hard limit leaves a small safety margin, the soft
// limit sits below the hard one by a bounded headroom.
func resolveContextBudget(contextWindow, maxOutputTokens int) contextBudget {
	if contextWindow <= 0 {
		contextWindow = DefaultContextWindowTokens
	}
	reservedOutput := maxOutputTokens
	if reservedOutput <= 0 {
		// A tiny window must not be eaten entirely by the minimum
		// reserve: cap it at a quarter of the window so a budget always
		// exists (a degenerate budget would silently disable
		// auto-compaction).
		reservedOutput = minInt(clampInt(contextWindow/8, reservedOutputMin, reservedOutputMax), contextWindow/4)
	}
	capacity := contextWindow - reservedOutput
	if capacity <= 0 {
		// A configured output limit larger than the window would leave
		// nothing; fall back to a quarter of the window.
		capacity = contextWindow - contextWindow/4
	}
	safety := clampInt((capacity*contextSafetyPercent+99)/100, contextSafetyMin, contextSafetyMax)
	// A tiny capacity must not be eaten entirely by the safety floor:
	// cap it at half the capacity so a budget never collapses to zero
	// (a zero budget would silently disable auto-compaction).
	if safety > capacity/2 {
		safety = capacity / 2
	}
	hard := capacity - safety
	if hard <= 0 {
		return contextBudget{}
	}
	headroom := clampInt(hard*softHeadroomPercent/100, softHeadroomMin, softHeadroomMax)
	soft := hard - headroom
	if soft <= 0 {
		soft = hard
	}
	return contextBudget{SoftInputLimit: soft, HardInputLimit: hard}
}

// compactionReclaimsEnough reports whether a compaction that shrank the
// estimated request from before to after is worth keeping: a summary that
// barely shrinks the context is churn, not relief.
func compactionReclaimsEnough(before, after int) bool {
	if before <= 0 || after <= 0 {
		return true
	}
	return before-after >= maxInt(2*1024, before/20)
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

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// compactionPressure grades the current request against the budget.
type compactionPressure int

const (
	pressureNone compactionPressure = iota
	pressureSoft
	pressureHard
)
