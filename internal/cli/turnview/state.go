package turnview

import (
	"fmt"
	"strings"

	"github.com/rexzhao/simple-agent/internal/execution"
)

type BlockKind string

const (
	BlockInput     BlockKind = "input"
	BlockReasoning BlockKind = "reasoning"
	BlockAssistant BlockKind = "assistant"
	BlockTool      BlockKind = "tool"
	BlockError     BlockKind = "error"
	BlockSystem    BlockKind = "system"
	BlockMailbox   BlockKind = "mailbox"
)

type Block struct {
	ID     string
	Kind   BlockKind
	TurnID string
	Title  string
	Text   string
	Status string
}

type StatusBarState struct {
	SessionID    string
	Provider     string
	ModelProfile string
	ModelID      string
	TurnID       string
	TurnStatus   string
	Mailbox      string

	InputTokens  int
	OutputTokens int
	TotalTokens  int
	ToolCount    int
}

type State struct {
	Blocks []Block
	Status StatusBarState

	blockIndex map[string]int
	nextID     int
}

func New() *State {
	return &State{}
}

func (s *State) SetSession(id, provider, modelProfile, modelID string) {
	s.Status.SessionID = strings.TrimSpace(id)
	s.Status.Provider = strings.TrimSpace(provider)
	s.Status.ModelProfile = strings.TrimSpace(modelProfile)
	s.Status.ModelID = strings.TrimSpace(modelID)
}

func (s *State) SetMailbox(text string) {
	s.Status.Mailbox = strings.TrimSpace(text)
}

func (s *State) AddInput(source, text string) {
	source = strings.TrimSpace(source)
	if source == "" {
		source = "user"
	}
	s.addBlock(Block{
		ID:     s.nextBlockID("input"),
		Kind:   BlockInput,
		Title:  source,
		Text:   strings.TrimRight(text, "\r\n"),
		Status: "queued",
	})
}

func (s *State) AddSystem(title, text, status string) {
	s.addBlock(Block{
		ID:     s.nextBlockID("system"),
		Kind:   BlockSystem,
		Title:  strings.TrimSpace(title),
		Text:   strings.TrimRight(text, "\r\n"),
		Status: strings.TrimSpace(status),
	})
}

func (s *State) AddMailboxStart(taskID string) {
	taskID = mailboxTaskID(taskID)
	s.addBlock(Block{
		ID:     "mailbox-start:" + taskID,
		Kind:   BlockMailbox,
		Title:  "mailbox task " + taskID + " started",
		Status: "running",
	})
}

func (s *State) AddMailboxEnd(taskID, status string) {
	taskID = mailboxTaskID(taskID)
	status = strings.TrimSpace(status)
	if status == "" {
		status = "finished"
	}
	s.addBlock(Block{
		ID:     "mailbox-end:" + taskID,
		Kind:   BlockMailbox,
		Title:  "mailbox task " + taskID + " " + status,
		Status: status,
	})
}

func (s *State) AddMessage(role, text string) {
	role = strings.TrimSpace(role)
	if role == "" {
		role = "message"
	}
	kind := BlockSystem
	switch role {
	case "user":
		kind = BlockInput
	case "assistant":
		kind = BlockAssistant
	}
	s.addBlock(Block{
		ID:    s.nextBlockID(role),
		Kind:  kind,
		Title: role,
		Text:  strings.TrimRight(text, "\r\n"),
	})
}

func (s *State) Apply(event execution.SessionStreamEvent) {
	eventType, _ := event["type"].(string)
	turnID := eventString(event, "turn_id")
	if turnID == "" {
		turnID = s.Status.TurnID
	}

	switch eventType {
	case "turn.started":
		s.Status.TurnID = turnID
		s.Status.TurnStatus = "running"
		s.Status.ToolCount = 0
	case "turn.committed":
		s.Status.TurnID = turnID
		s.Status.TurnStatus = "completed"
	case "turn.failed":
		s.Status.TurnID = turnID
		s.Status.TurnStatus = "failed"
		message := eventRawString(event, "message")
		if message == "" {
			message = "turn failed"
		}
		s.addBlock(Block{
			ID:     s.nextBlockID("error"),
			Kind:   BlockError,
			TurnID: turnID,
			Title:  "turn failed",
			Text:   message,
			Status: "failed",
		})
	case "reasoning.delta":
		text := eventRawString(event, "text")
		if text == "" {
			return
		}
		block := s.ensureBlock("reasoning:"+turnID, BlockReasoning, turnID, "reasoning")
		block.Text += text
		block.Status = "running"
	case "text.delta":
		text := eventRawString(event, "text")
		if text == "" {
			return
		}
		block := s.ensureBlock("assistant:"+turnID, BlockAssistant, turnID, "assistant")
		block.Text += text
		block.Status = "running"
	case "tool.started":
		name := eventString(event, "name")
		if name == "" {
			return
		}
		id := toolBlockID(eventString(event, "tool_call_id"), turnID, name)
		block := s.ensureBlock(id, BlockTool, turnID, name)
		if block.Status == "" {
			s.Status.ToolCount++
		}
		block.Status = "running"
	case "tool.finished":
		name := eventString(event, "name")
		if name == "" {
			return
		}
		id := toolBlockID(eventString(event, "tool_call_id"), turnID, name)
		block := s.ensureBlock(id, BlockTool, turnID, name)
		if eventBool(event, "is_error") {
			block.Status = "failed"
		} else {
			block.Status = "completed"
		}
	case "usage.updated":
		s.Status.TurnID = turnID
		s.Status.InputTokens = eventInt(event, "input_tokens")
		s.Status.OutputTokens = eventInt(event, "output_tokens")
		s.Status.TotalTokens = eventInt(event, "total_tokens")
	case "compaction.created":
		s.AddSystem("compaction created", "", "completed")
	case "active_history.replaced":
		s.AddSystem("active history replaced", "", "completed")
	}
}

func (s *State) Snapshot() State {
	out := State{
		Blocks: make([]Block, len(s.Blocks)),
		Status: s.Status,
		nextID: s.nextID,
	}
	copy(out.Blocks, s.Blocks)
	return out
}

func (s *State) ensureBlock(id string, kind BlockKind, turnID, title string) *Block {
	s.ensureIndex()
	if index, ok := s.blockIndex[id]; ok {
		return &s.Blocks[index]
	}
	s.Blocks = append(s.Blocks, Block{
		ID:     id,
		Kind:   kind,
		TurnID: turnID,
		Title:  strings.TrimSpace(title),
	})
	s.blockIndex[id] = len(s.Blocks) - 1
	return &s.Blocks[len(s.Blocks)-1]
}

func (s *State) addBlock(block Block) {
	s.ensureIndex()
	if block.ID == "" {
		block.ID = s.nextBlockID("block")
	}
	s.Blocks = append(s.Blocks, block)
	s.blockIndex[block.ID] = len(s.Blocks) - 1
}

func (s *State) ensureIndex() {
	if s.blockIndex != nil {
		return
	}
	s.blockIndex = make(map[string]int, len(s.Blocks))
	for i, block := range s.Blocks {
		if block.ID != "" {
			s.blockIndex[block.ID] = i
		}
	}
}

func (s *State) nextBlockID(prefix string) string {
	s.nextID++
	return fmt.Sprintf("%s:%d", prefix, s.nextID)
}

func eventString(event execution.SessionStreamEvent, key string) string {
	value, _ := event[key].(string)
	return strings.TrimSpace(value)
}

func eventRawString(event execution.SessionStreamEvent, key string) string {
	value, _ := event[key].(string)
	return value
}

func eventBool(event execution.SessionStreamEvent, key string) bool {
	value, _ := event[key].(bool)
	return value
}

func eventInt(event execution.SessionStreamEvent, key string) int {
	switch value := event[key].(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	default:
		return 0
	}
}

func toolBlockID(toolCallID, turnID, name string) string {
	if strings.TrimSpace(toolCallID) != "" {
		return "tool:" + strings.TrimSpace(toolCallID)
	}
	return "tool:" + strings.TrimSpace(turnID) + ":" + strings.TrimSpace(name)
}

func mailboxTaskID(taskID string) string {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return "(unknown)"
	}
	return taskID
}
