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
	prepareRequest(body map[string]any, request model.Request, stream bool)
	prepareMessage(item map[string]any, message model.Message)
	usage(chunk chatCompletionChunk) *chatCompletionUsage
}

type openAICompatibility struct{}

func (openAICompatibility) prepareRequest(map[string]any, model.Request, bool) {}

func (openAICompatibility) prepareMessage(map[string]any, model.Message) {}

func (openAICompatibility) usage(chunk chatCompletionChunk) *chatCompletionUsage {
	return chunk.Usage
}

type kimiCompatibility struct {
	openAICompatibility
}

func (kimiCompatibility) prepareRequest(body map[string]any, request model.Request, stream bool) {
	if _, configured := body["prompt_cache_key"]; !configured {
		if sessionID := strings.TrimSpace(request.SessionID); sessionID != "" {
			body["prompt_cache_key"] = sessionID
		}
	}
	if !stream {
		return
	}

	rawOptions, configured := body["stream_options"]
	if !configured {
		body["stream_options"] = map[string]any{"include_usage": true}
		return
	}
	options, ok := rawOptions.(map[string]any)
	if !ok {
		return
	}
	options = copyAnyMap(options)
	if _, configured := options["include_usage"]; !configured {
		options["include_usage"] = true
	}
	body["stream_options"] = options
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

func copyAnyMap(values map[string]any) map[string]any {
	copied := make(map[string]any, len(values))
	for key, value := range values {
		copied[key] = value
	}
	return copied
}
