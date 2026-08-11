package openaichat

import (
	"fmt"
	"strings"

	"github.com/rexzhao/simple-agent/internal/model"
)

const (
	CompatibilityOpenAI = "openai"
	CompatibilityKimi   = "kimi"
)

type chatCompatibility interface {
	prepareRequest(body map[string]any, request model.Request)
	prepareMessage(item map[string]any, message model.Message)
	usage(chunk chatCompletionChunk) *chatCompletionUsage
}

type openAICompatibility struct{}

func (openAICompatibility) prepareRequest(map[string]any, model.Request) {}

func (openAICompatibility) prepareMessage(map[string]any, model.Message) {}

func (openAICompatibility) usage(chunk chatCompletionChunk) *chatCompletionUsage {
	return chunk.Usage
}

type kimiCompatibility struct {
	openAICompatibility
}

func (kimiCompatibility) prepareRequest(body map[string]any, request model.Request) {
	if _, configured := body["prompt_cache_key"]; !configured {
		if sessionID := strings.TrimSpace(request.SessionID); sessionID != "" {
			body["prompt_cache_key"] = sessionID
		}
	}
}

func (kimiCompatibility) prepareMessage(item map[string]any, message model.Message) {
	if message.Role == model.MessageRoleAssistant {
		item["reasoning_content"] = message.ReasoningContent
	}
}

func (kimiCompatibility) usage(chunk chatCompletionChunk) *chatCompletionUsage {
	if chunk.Usage != nil {
		return chunk.Usage
	}
	for _, choice := range chunk.Choices {
		if choice.Usage != nil {
			return choice.Usage
		}
	}
	return nil
}

func resolveCompatibility(value string) (chatCompatibility, error) {
	switch normalized := strings.ToLower(strings.TrimSpace(value)); normalized {
	case "", CompatibilityOpenAI:
		return openAICompatibility{}, nil
	case CompatibilityKimi:
		return kimiCompatibility{}, nil
	default:
		return nil, fmt.Errorf("unsupported OpenAI Chat compatibility %q", value)
	}
}
