package openairesponses

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rexzhao/simple-agent/internal/model"
)

func BuildRequestBody(request model.Request, stream bool) ([]byte, error) {
	body, _, err := buildRequestBody(request, stream)
	return body, err
}

func buildRequestBody(request model.Request, stream bool) ([]byte, *toolNameMapper, error) {
	return buildRequestBodyWithOptions(request, stream, requestBodyOptions{})
}

type requestBodyOptions struct {
	forceStoreFalse     bool
	origin              string
	disableContinuation bool
}

func buildRequestBodyWithOptions(request model.Request, stream bool, options requestBodyOptions) ([]byte, *toolNameMapper, error) {
	body, toolNames, _, err := buildProviderRequest(request, stream, options)
	return body, toolNames, err
}

type providerRequestMetadata struct {
	Store            bool
	CacheKey         string
	SessionAffinity  string
	UsedContinuation bool
}

type responsesParameterOptions struct {
	Store *bool                       `json:"store"`
	State string                      `json:"state"`
	Cache responsesPromptCacheOptions `json:"cache"`
}

type responsesPromptCacheOptions struct {
	Enabled         *bool  `json:"enabled"`
	Key             string `json:"key"`
	Capability      string `json:"capability"`
	Mode            string `json:"mode"`
	TTL             string `json:"ttl"`
	Retention       string `json:"retention"`
	Breakpoint      string `json:"breakpoint"`
	SessionAffinity string `json:"session_affinity"`
}

type responseInputOptions struct {
	origin                    string
	model                     string
	markInstructionBreakpoint bool
	allowCacheBreakpoints     bool
}

const openAIResponsesMinOutputTokens = 16

func buildProviderRequest(request model.Request, stream bool, options requestBodyOptions) ([]byte, *toolNameMapper, providerRequestMetadata, error) {
	toolNames := newToolNameMapper(request.Tools)
	responsesOptions, err := parseResponsesParameterOptions(request.Parameters)
	if err != nil {
		return nil, nil, providerRequestMetadata{}, err
	}
	body, err := buildParameters(request.Parameters, toolNames)
	if err != nil {
		return nil, nil, providerRequestMetadata{}, err
	}

	store, err := effectiveStore(body, responsesOptions, options.forceStoreFalse)
	if err != nil {
		return nil, nil, providerRequestMetadata{}, err
	}
	body["store"] = store

	capability, err := promptCacheCapability(request.Model, responsesOptions.Cache.Capability)
	if err != nil {
		return nil, nil, providerRequestMetadata{}, err
	}
	cacheEnabled := responsesOptions.Cache.Enabled == nil || *responsesOptions.Cache.Enabled
	markInstructionBreakpoint := cacheEnabled && strings.EqualFold(strings.TrimSpace(responsesOptions.Cache.Breakpoint), "instructions")
	inputOptions := responseInputOptions{
		origin:                    options.origin,
		model:                     request.Model,
		markInstructionBreakpoint: markInstructionBreakpoint,
		allowCacheBreakpoints:     cacheEnabled && capability == "modern",
	}

	inputMessages := request.Messages
	metadata := providerRequestMetadata{Store: store}
	stateMode := strings.ToLower(strings.TrimSpace(responsesOptions.State))
	if stateMode == "" {
		stateMode = "manual"
	}
	if stateMode != "manual" && stateMode != "previous_response_id" {
		return nil, nil, providerRequestMetadata{}, fmt.Errorf("OpenAI Responses responses.state must be manual or previous_response_id")
	}
	if stateMode == "previous_response_id" {
		if !store {
			return nil, nil, providerRequestMetadata{}, fmt.Errorf("OpenAI Responses previous_response_id state requires store: true")
		}
		if !options.disableContinuation {
			if responseID, start := continuationPoint(request.Messages, options.origin, request.Model); responseID != "" {
				body["previous_response_id"] = responseID
				inputMessages = request.Messages[start:]
				metadata.UsedContinuation = true
			}
		}
	}

	input, breakpointCount, err := buildInput(inputMessages, toolNames, inputOptions)
	if err != nil {
		return nil, nil, providerRequestMetadata{}, err
	}
	if err := applyPromptCacheOptions(body, request, responsesOptions.Cache, capability, cacheEnabled, breakpointCount, &metadata); err != nil {
		return nil, nil, providerRequestMetadata{}, err
	}

	if options.forceStoreFalse {
		body["store"] = false
	}
	body["model"] = request.Model
	body["input"] = input
	body["stream"] = stream
	if len(request.Tools) > 0 {
		body["tools"] = buildTools(request.Tools, toolNames)
	}

	data, err := json.Marshal(body)
	if err != nil {
		return nil, nil, providerRequestMetadata{}, err
	}
	return data, toolNames, metadata, nil
}

func buildParameters(parameters map[string]any, toolNames *toolNameMapper) (map[string]any, error) {
	body := make(map[string]any, len(parameters)+3)
	for key, value := range parameters {
		if key == "responses" {
			continue
		}
		if isUnsupportedParameter(key) {
			return nil, fmt.Errorf("OpenAI Responses adapter does not support parameter %q", key)
		}
		if key == "max_tokens" {
			if _, ok := parameters["max_output_tokens"]; !ok {
				body["max_output_tokens"] = clampMinimumOutputTokens(value)
			}
			continue
		}
		if key == "max_output_tokens" {
			body[key] = clampMinimumOutputTokens(value)
			continue
		}
		if key == "tool_choice" {
			body[key] = mapToolChoice(value, toolNames)
			continue
		}
		body[key] = value
	}
	return body, nil
}

func isUnsupportedParameter(key string) bool {
	switch key {
	case "tools",
		"tool_resources",
		"tool_outputs",
		"functions",
		"function_call",
		"function_call_output",
		"function_call_outputs",
		"web_search_options":
		return true
	default:
		return false
	}
}

func mapToolChoice(value any, toolNames *toolNameMapper) any {
	choice, ok := value.(map[string]any)
	if !ok {
		return value
	}
	choiceType, _ := choice["type"].(string)
	if choiceType == "allowed_tools" {
		tools, ok := choice["tools"].([]any)
		if !ok {
			return value
		}
		out := copyMap(choice)
		outTools := make([]any, 0, len(tools))
		for _, tool := range tools {
			outTools = append(outTools, mapToolChoiceTool(tool, toolNames))
		}
		out["tools"] = outTools
		return out
	}

	name, ok := choice["name"].(string)
	if !ok || choiceType != "function" {
		return value
	}

	out := copyMap(choice)
	out["name"] = toolNames.responsesName(name)
	return out
}

func mapToolChoiceTool(value any, toolNames *toolNameMapper) any {
	tool, ok := value.(map[string]any)
	if !ok {
		return value
	}
	if toolType, _ := tool["type"].(string); toolType != "function" {
		return value
	}
	name, ok := tool["name"].(string)
	if !ok {
		return value
	}

	out := copyMap(tool)
	out["name"] = toolNames.responsesName(name)
	return out
}

func copyMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func buildInput(messages []model.Message, toolNames *toolNameMapper, options responseInputOptions) ([]map[string]any, int, error) {
	out := make([]map[string]any, 0, len(messages))
	breakpointCount := 0
	markIndex := -1
	if options.markInstructionBreakpoint {
		markIndex = stableInstructionBreakpointIndex(messages)
		if markIndex < 0 {
			return nil, 0, fmt.Errorf("OpenAI Responses explicit instruction cache breakpoint requires a leading system or developer message")
		}
	}
	for index, message := range messages {
		switch message.Role {
		case model.MessageRoleSystem, model.MessageRoleDeveloper, model.MessageRoleUser:
			content, count, err := buildMessageInputContent(message, index == markIndex, options.allowCacheBreakpoints)
			if err != nil {
				return nil, 0, err
			}
			breakpointCount += count
			out = append(out, map[string]any{
				"role":    string(message.Role),
				"content": content,
			})
		case model.MessageRoleAssistant:
			out = append(out, buildAssistantInput(message, toolNames, options)...)
		case model.MessageRoleTool:
			out = append(out, map[string]any{
				"type":    "function_call_output",
				"call_id": message.ToolCallID,
				"output":  message.Content,
			})
		default:
			return nil, 0, fmt.Errorf("unsupported OpenAI Responses role %q", message.Role)
		}
	}
	return out, breakpointCount, nil
}

func buildAssistantInput(message model.Message, toolNames *toolNameMapper, options responseInputOptions) []map[string]any {
	stateMatches := responseStateMatches(message.ResponseState, options.origin, options.model)
	out := make([]map[string]any, 0, len(message.ToolCalls)+2)
	if stateMatches {
		for _, reasoning := range message.ResponseState.ReasoningItems {
			var item map[string]any
			if json.Unmarshal(reasoning, &item) == nil && len(item) > 0 {
				out = append(out, item)
			}
		}
	}
	if message.Content != "" || len(message.ToolCalls) == 0 {
		if stateMatches && message.ResponseState.MessageID != "" {
			item := map[string]any{
				"type":   "message",
				"id":     message.ResponseState.MessageID,
				"role":   "assistant",
				"status": "completed",
				"content": []map[string]any{{
					"type":        "output_text",
					"text":        message.Content,
					"annotations": []any{},
				}},
			}
			if message.ResponseState.MessagePhase != "" {
				item["phase"] = message.ResponseState.MessagePhase
			}
			out = append(out, item)
		} else {
			out = append(out, map[string]any{
				"role":    string(message.Role),
				"content": message.Content,
			})
		}
	}
	for _, toolCall := range message.ToolCalls {
		out = append(out, buildFunctionCallInput(toolCall, toolNames, stateMatches))
	}
	return out
}

func buildFunctionCallInput(toolCall model.ToolCall, toolNames *toolNameMapper, includeProviderID bool) map[string]any {
	item := map[string]any{
		"type":      "function_call",
		"call_id":   toolCall.ID,
		"name":      toolNames.responsesName(toolCall.Name),
		"arguments": responseToolCallArguments(toolCall.Arguments),
	}
	if includeProviderID && toolCall.ProviderID != "" {
		item["id"] = toolCall.ProviderID
	}
	return item
}

func responseToolCallArguments(arguments string) string {
	if strings.TrimSpace(arguments) == "" {
		return "{}"
	}
	return arguments
}

func parseResponsesParameterOptions(parameters map[string]any) (responsesParameterOptions, error) {
	raw, ok := parameters["responses"]
	if !ok || raw == nil {
		return responsesParameterOptions{}, nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return responsesParameterOptions{}, fmt.Errorf("encode OpenAI Responses responses options: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var options responsesParameterOptions
	if err := decoder.Decode(&options); err != nil {
		return responsesParameterOptions{}, fmt.Errorf("parse OpenAI Responses responses options: %w", err)
	}
	return options, nil
}

func effectiveStore(body map[string]any, options responsesParameterOptions, forceStoreFalse bool) (bool, error) {
	store := false
	if configured, ok := body["store"]; ok {
		value, ok := configured.(bool)
		if !ok {
			return false, fmt.Errorf("OpenAI Responses store must be a boolean")
		}
		store = value
	}
	if options.Store != nil {
		store = *options.Store
	}
	if forceStoreFalse {
		store = false
	}
	return store, nil
}

func promptCacheCapability(modelID, configured string) (string, error) {
	capability := strings.ToLower(strings.TrimSpace(configured))
	if capability == "" || capability == "auto" {
		modelID = strings.ToLower(strings.TrimSpace(modelID))
		if modelID == "gpt-5.6" || strings.HasPrefix(modelID, "gpt-5.6-") {
			return "modern", nil
		}
		return "legacy", nil
	}
	switch capability {
	case "modern", "legacy", "disabled":
		return capability, nil
	default:
		return "", fmt.Errorf("OpenAI Responses cache.capability must be auto, modern, legacy, or disabled")
	}
}

func applyPromptCacheOptions(body map[string]any, request model.Request, options responsesPromptCacheOptions, capability string, enabled bool, breakpointCount int, metadata *providerRequestMetadata) error {
	if !enabled {
		delete(body, "prompt_cache_key")
		delete(body, "prompt_cache_options")
		delete(body, "prompt_cache_retention")
		return nil
	}

	if capability == "disabled" {
		if promptCacheOptionsConfigured(options) {
			return fmt.Errorf("OpenAI Responses prompt cache options are disabled for this model profile")
		}
		return nil
	}

	key := strings.TrimSpace(options.Key)
	if key == "" {
		if configured, ok := body["prompt_cache_key"]; ok {
			var valid bool
			key, valid = configured.(string)
			if !valid {
				return fmt.Errorf("OpenAI Responses prompt_cache_key must be a string")
			}
		}
	}
	if key == "" {
		key = strings.TrimSpace(request.SessionID)
	}
	key = clampPromptCacheKey(key)
	if key != "" {
		body["prompt_cache_key"] = key
		metadata.CacheKey = key
	}

	mode := strings.ToLower(strings.TrimSpace(options.Mode))
	ttl := strings.ToLower(strings.TrimSpace(options.TTL))
	retention := strings.ToLower(strings.TrimSpace(options.Retention))
	breakpoint := strings.ToLower(strings.TrimSpace(options.Breakpoint))
	if breakpoint != "" && breakpoint != "instructions" {
		return fmt.Errorf("OpenAI Responses cache.breakpoint must be instructions when set")
	}

	switch capability {
	case "modern":
		if retention != "" {
			return fmt.Errorf("OpenAI Responses GPT-5.6 cache uses cache.ttl instead of cache.retention")
		}
		if mode != "" && mode != "implicit" && mode != "explicit" {
			return fmt.Errorf("OpenAI Responses cache.mode must be implicit or explicit")
		}
		if ttl != "" && ttl != "30m" {
			return fmt.Errorf("OpenAI Responses cache.ttl currently supports only 30m")
		}
		cacheOptions := mapFromAny(body["prompt_cache_options"])
		if mode != "" {
			cacheOptions["mode"] = mode
		}
		if ttl != "" {
			cacheOptions["ttl"] = ttl
		}
		if len(cacheOptions) > 0 {
			body["prompt_cache_options"] = cacheOptions
		}
		effectiveMode := effectivePromptCacheMode(cacheOptions)
		if effectiveMode == "explicit" && breakpointCount == 0 {
			return fmt.Errorf("OpenAI Responses explicit cache mode requires at least one prompt_cache_breakpoint")
		}
		if breakpointCount > 0 && effectiveMode != "explicit" {
			return fmt.Errorf("OpenAI Responses prompt_cache_breakpoint requires explicit cache mode")
		}
	case "legacy":
		if mode != "" || ttl != "" || breakpoint != "" || breakpointCount > 0 {
			return fmt.Errorf("OpenAI Responses cache mode, ttl, and breakpoints require a GPT-5.6-compatible model")
		}
		if retention != "" {
			if retention != "in_memory" && retention != "24h" {
				return fmt.Errorf("OpenAI Responses cache.retention must be in_memory or 24h")
			}
			body["prompt_cache_retention"] = retention
		}
	}

	affinity := strings.ToLower(strings.TrimSpace(options.SessionAffinity))
	if affinity == "" {
		affinity = "auto"
	}
	switch affinity {
	case "auto", "openai", "openrouter", "none":
		metadata.SessionAffinity = affinity
	default:
		return fmt.Errorf("OpenAI Responses cache.session_affinity must be auto, openai, openrouter, or none")
	}
	return nil
}

func promptCacheOptionsConfigured(options responsesPromptCacheOptions) bool {
	return strings.TrimSpace(options.Key) != "" ||
		strings.TrimSpace(options.Mode) != "" ||
		strings.TrimSpace(options.TTL) != "" ||
		strings.TrimSpace(options.Retention) != "" ||
		strings.TrimSpace(options.Breakpoint) != "" ||
		strings.TrimSpace(options.SessionAffinity) != ""
}

func mapFromAny(value any) map[string]any {
	configured, ok := value.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return copyMap(configured)
}

func effectivePromptCacheMode(options map[string]any) string {
	mode, _ := options["mode"].(string)
	return strings.ToLower(strings.TrimSpace(mode))
}

func clampPromptCacheKey(key string) string {
	chars := []rune(key)
	if len(chars) <= 64 {
		return key
	}
	return string(chars[:64])
}

func stableInstructionBreakpointIndex(messages []model.Message) int {
	last := -1
	for index, message := range messages {
		if message.Role != model.MessageRoleSystem && message.Role != model.MessageRoleDeveloper {
			break
		}
		last = index
	}
	return last
}

func buildMessageInputContent(message model.Message, markBreakpoint, allowBreakpoints bool) (any, int, error) {
	if len(message.ContentBlocks) == 0 && !markBreakpoint {
		return message.Content, 0, nil
	}
	if len(message.ContentBlocks) > 0 && message.Content != "" {
		return nil, 0, fmt.Errorf("OpenAI Responses message cannot set both content and content blocks")
	}

	blocks := message.ContentBlocks
	if len(blocks) == 0 {
		blocks = []model.InputContentBlock{{Type: "input_text", Text: message.Content}}
	}
	out := make([]map[string]any, 0, len(blocks))
	breakpoints := 0
	for index, block := range blocks {
		typeName := strings.TrimSpace(block.Type)
		if typeName == "" {
			typeName = "input_text"
		}
		item := map[string]any{"type": typeName}
		switch typeName {
		case "input_text":
			item["text"] = block.Text
		case "input_image":
			if strings.TrimSpace(block.ImageURL) == "" {
				return nil, 0, fmt.Errorf("OpenAI Responses input_image requires image_url")
			}
			item["image_url"] = block.ImageURL
			detail := strings.TrimSpace(block.Detail)
			if detail == "" {
				detail = "auto"
			}
			item["detail"] = detail
		case "input_file":
			if strings.TrimSpace(block.FileID) == "" {
				return nil, 0, fmt.Errorf("OpenAI Responses input_file requires file_id")
			}
			item["file_id"] = block.FileID
		default:
			return nil, 0, fmt.Errorf("unsupported OpenAI Responses content block type %q", typeName)
		}

		hasBreakpoint := block.PromptCacheBreakpoint || (markBreakpoint && index == len(blocks)-1)
		if hasBreakpoint {
			if !allowBreakpoints {
				return nil, 0, fmt.Errorf("OpenAI Responses prompt_cache_breakpoint requires a GPT-5.6-compatible model")
			}
			item["prompt_cache_breakpoint"] = map[string]any{"mode": "explicit"}
			breakpoints++
		}
		out = append(out, item)
	}
	return out, breakpoints, nil
}

func continuationPoint(messages []model.Message, origin, modelID string) (string, int) {
	for index := len(messages) - 1; index >= 0; index-- {
		state := messages[index].ResponseState
		if messages[index].Role != model.MessageRoleAssistant || state == nil || !state.Stored || strings.TrimSpace(state.ID) == "" {
			continue
		}
		if !responseStateMatches(state, origin, modelID) {
			continue
		}
		return state.ID, index + 1
	}
	return "", 0
}

func responseStateMatches(state *model.ResponseState, origin, modelID string) bool {
	return state != nil && strings.TrimSpace(state.Origin) == strings.TrimSpace(origin) && strings.TrimSpace(state.Model) == strings.TrimSpace(modelID)
}

func clampMinimumOutputTokens(value any) any {
	switch value := value.(type) {
	case int:
		if value > 0 && value < openAIResponsesMinOutputTokens {
			return openAIResponsesMinOutputTokens
		}
	case int64:
		if value > 0 && value < openAIResponsesMinOutputTokens {
			return int64(openAIResponsesMinOutputTokens)
		}
	case float64:
		if value > 0 && value < openAIResponsesMinOutputTokens {
			return float64(openAIResponsesMinOutputTokens)
		}
	case json.Number:
		if parsed, err := value.Int64(); err == nil && parsed > 0 && parsed < openAIResponsesMinOutputTokens {
			return json.Number("16")
		}
	}
	return value
}

func buildTools(tools []model.Tool, toolNames *toolNameMapper) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		out = append(out, map[string]any{
			"type":        "function",
			"name":        toolNames.responsesName(tool.Name),
			"description": tool.Description,
			"parameters":  responsesInputSchema(tool.InputSchema),
		})
	}
	return out
}

func responsesInputSchema(schema map[string]any) map[string]any {
	if len(schema) > 0 {
		return schema
	}
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}

type toolNameMapper struct {
	toResponses map[string]string
	toInternal  map[string]string
	used        map[string]struct{}
	reserved    map[string]struct{}
	nextAlias   int
}

func newToolNameMapper(tools []model.Tool) *toolNameMapper {
	mapper := &toolNameMapper{
		toResponses: make(map[string]string, len(tools)),
		toInternal:  make(map[string]string, len(tools)),
		used:        make(map[string]struct{}, len(tools)),
		reserved:    make(map[string]struct{}, len(tools)),
	}
	for _, tool := range tools {
		if isValidOpenAIResponsesToolName(tool.Name) {
			mapper.reserved[tool.Name] = struct{}{}
		}
	}
	for _, tool := range tools {
		mapper.mapToolName(tool.Name)
	}
	return mapper
}

func (m *toolNameMapper) responsesName(internalName string) string {
	if m == nil {
		return internalName
	}
	if name, ok := m.toResponses[internalName]; ok {
		return name
	}
	return m.mapToolName(internalName)
}

func (m *toolNameMapper) internalName(responsesName string) string {
	if m == nil {
		return responsesName
	}
	if name, ok := m.toInternal[responsesName]; ok {
		return name
	}
	return responsesName
}

func (m *toolNameMapper) mapToolName(internalName string) string {
	if name, ok := m.toResponses[internalName]; ok {
		return name
	}

	name := internalName
	if !isValidOpenAIResponsesToolName(name) || m.isUsed(name) {
		name = m.nextToolAlias()
	}
	m.toResponses[internalName] = name
	m.toInternal[name] = internalName
	m.used[name] = struct{}{}
	return name
}

func (m *toolNameMapper) nextToolAlias() string {
	for {
		name := fmt.Sprintf("tool_%d", m.nextAlias)
		m.nextAlias++
		if m.isUsed(name) {
			continue
		}
		if _, ok := m.reserved[name]; ok {
			continue
		}
		return name
	}
}

func (m *toolNameMapper) isUsed(name string) bool {
	_, ok := m.used[name]
	return ok
}

func isValidOpenAIResponsesToolName(name string) bool {
	if len(name) == 0 || len(name) > 64 {
		return false
	}
	for _, char := range name {
		switch {
		case char >= 'a' && char <= 'z':
		case char >= 'A' && char <= 'Z':
		case char >= '0' && char <= '9':
		case char == '_' || char == '-':
		default:
			return false
		}
	}
	return true
}
