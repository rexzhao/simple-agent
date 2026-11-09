package tools

import (
	"context"
	"fmt"
	"strings"
)

func askUserQuestionTool() *ToolDef {
	return &ToolDef{
		Name: "ask_user_question",
		Description: "Ask brief, direct, self-contained questions about missing information, " +
			"preferences, or decisions. The conversation pauses until the user answers or skips.",
		Parameters: schema(map[string]any{
			"questions": map[string]any{
				"type":        "array",
				"description": "Questions to ask the user before continuing.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id": map[string]any{"type": "string",
							"description": "Stable id for this question; echoed in the answer."},
						"question": map[string]any{"type": "string",
							"description": "The specific question to ask the user."},
						"header": map[string]any{"type": "string",
							"description": `Optional short heading for the question, such as "Confirm" or "Choose Mode".`},
						"options": map[string]any{
							"type": "array",
							"description": "Optional choices to show the user. If you recommend one, put it first " +
								`and append "(Recommended)" to that label.`,
							"items": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"label": map[string]any{"type": "string",
										"description": "Short user-facing option label."},
									"description": map[string]any{"type": "string",
										"description": "One sentence explaining the tradeoff or impact."},
								},
								"required": []string{"label"},
							},
						},
						"multiSelect": map[string]any{"type": "boolean",
							"description": "Whether the user may select more than one option. Defaults to false."},
					},
					"required": []string{"id", "question"},
				},
			},
		}, "questions"),
		Run: askUserQuestionRun,
	}
}

func askUserQuestionRun(_ context.Context, args Args, _ *ToolContext) (ToolReturn, error) {
	items, err := args.Items("questions")
	if err != nil {
		return ToolReturn{}, err
	}
	if len(items) == 0 {
		return ToolReturn{}, fmt.Errorf("questions 不能为空")
	}

	questions := make([]QuestionSpec, 0, len(items))
	seen := make(map[string]bool, len(items))
	for i, item := range items {
		id, _ := item["id"].(string)
		id = strings.TrimSpace(id)
		if id == "" {
			return ToolReturn{}, fmt.Errorf("questions[%d].id 不能为空", i)
		}
		if seen[id] {
			return ToolReturn{}, fmt.Errorf("questions[%d].id 重复：%s", i, id)
		}
		seen[id] = true

		question, _ := item["question"].(string)
		question = strings.TrimSpace(question)
		if question == "" {
			return ToolReturn{}, fmt.Errorf("questions[%d].question 不能为空", i)
		}

		var options []QuestionOption
		if rawOptions, ok := item["options"].([]any); ok {
			for _, raw := range rawOptions {
				obj, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				label, _ := obj["label"].(string)
				if label == "" {
					continue
				}
				description, _ := obj["description"].(string)
				options = append(options, QuestionOption{Label: label, Description: description})
			}
		}

		header, _ := item["header"].(string)
		// 模型有时写 multi_select，两种都认
		multi := false
		if v, ok := item["multiSelect"].(bool); ok {
			multi = v
		} else if v, ok := item["multi_select"].(bool); ok {
			multi = v
		}

		questions = append(questions, QuestionSpec{
			ID:          id,
			Question:    question,
			Header:      header,
			Options:     options,
			MultiSelect: multi,
		})
	}

	return AskOf(questions), nil
}
