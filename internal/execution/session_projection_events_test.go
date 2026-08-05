package execution

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/rexzhao/simple-agent/internal/model"
	"github.com/rexzhao/simple-agent/internal/sessions"
)

func TestSessionProjectionItemEventsCarryCommittedSafeDTO(t *testing.T) {
	service, _, session := newExecutionServiceWithSession(t, t.TempDir(), nil)

	user := sessions.SessionItemFromMessage("user-visible", model.Message{
		Role:    model.MessageRoleUser,
		Content: "hello from the user",
	})
	user.TurnID = "turn-1"
	user, err := service.sessionStore.AppendItem(session.ID, user)
	if err != nil {
		t.Fatalf("AppendItem(user) error = %v", err)
	}
	userState, err := service.sessionStore.LoadState(session.ID)
	if err != nil {
		t.Fatalf("LoadState(after user) error = %v", err)
	}
	userEvent := persistedItemEvent(t, service, session.ID, sessions.RecordTypeItemAppended, user.ID)
	appended, ok := service.sessionStreamEventFromPersistedEvent(session.ID, "run-1", "turn-1", userEvent, userState.LastSeq)
	if !ok {
		t.Fatal("user item projection event was skipped")
	}
	assertProjectionEnvelope(t, appended, "item.created", session.ID, "run-1", "turn-1", userEvent.Seq, userState.LastSeq, user.Seq, user.ID)
	userDTO, ok := appended["item"].(SessionItem)
	if !ok {
		t.Fatalf("appended item = %T, want SessionItem", appended["item"])
	}
	if userDTO.Message == nil || userDTO.Message.Content == nil || userDTO.Message.Content.Inline != "hello from the user" {
		t.Fatalf("user DTO = %#v, want committed user text", userDTO)
	}

	// Image payloads are persisted as blobs, but the stream DTO exposes only
	// safe reference metadata and never the data URL or bytes.
	image := sessions.SessionItemFromMessage("user-image", model.Message{
		Role: model.MessageRoleUser,
		ContentBlocks: []model.InputContentBlock{{
			Type:     "input_image",
			ImageURL: model.ImageDataURL("image/png", []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}),
		}},
	})
	image.TurnID = "turn-1"
	image, err = service.sessionStore.AppendItem(session.ID, image)
	if err != nil {
		t.Fatalf("AppendItem(image) error = %v", err)
	}
	imageState, err := service.sessionStore.LoadState(session.ID)
	if err != nil {
		t.Fatalf("LoadState(after image) error = %v", err)
	}
	imageEvent := persistedItemEvent(t, service, session.ID, sessions.RecordTypeItemAppended, image.ID)
	imageStreamEvent, ok := service.sessionStreamEventFromPersistedEvent(session.ID, "run-1", "turn-1", imageEvent, imageState.LastSeq)
	if !ok {
		t.Fatal("image item projection event was skipped")
	}
	imageDTO, ok := imageStreamEvent["item"].(SessionItem)
	if !ok || imageDTO.Message == nil || len(imageDTO.Message.Images) != 1 {
		t.Fatalf("image DTO = %#v, want one safe image reference", imageStreamEvent["item"])
	}
	if imageDTO.Message.Images[0].MediaType != "image/png" || imageDTO.Message.Images[0].SizeBytes != 8 || imageDTO.Message.Images[0].Hash == "" {
		t.Fatalf("image attachment DTO = %#v, want normalized metadata", imageDTO.Message.Images[0])
	}
	encoded, err := json.Marshal(imageStreamEvent)
	if err != nil {
		t.Fatalf("marshal image projection event: %v", err)
	}
	if strings.Contains(string(encoded), "data:image") || strings.Contains(string(encoded), "iVBOR") {
		t.Fatalf("image projection event leaked inline image data: %s", encoded)
	}

	showReasoningState, err := service.sessionStore.LoadState(session.ID)
	if err != nil {
		t.Fatalf("LoadState(before reasoning projection) error = %v", err)
	}
	showReasoningState.ShowReasoning = true
	if _, err := service.sessionStore.SaveMetadata(showReasoningState); err != nil {
		t.Fatalf("SaveMetadata(show reasoning) error = %v", err)
	}

	assistant := sessions.SessionItemFromMessage("assistant-tool-call", model.Message{
		Role:             model.MessageRoleAssistant,
		Content:          "I will inspect the file",
		ReasoningContent: "First I will inspect the repository.",
		ToolCalls: []model.ToolCall{{
			ID:        "call-1",
			Name:      "read_file",
			Arguments: `{"path":"README.md","secret":"provider-private"}`,
		}},
	})
	assistant.TurnID = "turn-1"
	assistant, err = service.sessionStore.AppendItem(session.ID, assistant)
	if err != nil {
		t.Fatalf("AppendItem(assistant) error = %v", err)
	}
	assistantState, err := service.sessionStore.LoadState(session.ID)
	if err != nil {
		t.Fatalf("LoadState(after assistant) error = %v", err)
	}
	assistantEvent := persistedItemEvent(t, service, session.ID, sessions.RecordTypeItemAppended, assistant.ID)
	assistantStreamEvent, ok := service.sessionStreamEventFromPersistedEvent(session.ID, "run-1", "turn-1", assistantEvent, assistantState.LastSeq)
	if !ok {
		t.Fatal("assistant item projection event was skipped")
	}
	if assistantStreamEvent["type"] != "item.created" {
		t.Fatalf("assistant projection type = %#v, want item.created", assistantStreamEvent["type"])
	}
	if assistantStreamEvent["agent_iteration"] != 0 {
		t.Fatalf("assistant projection agent_iteration = %#v, want the explicit zero for this fixture", assistantStreamEvent["agent_iteration"])
	}
	assistantDTO := assistantStreamEvent["item"].(SessionItem)
	if assistantDTO.Message == nil || len(assistantDTO.Message.ToolCalls) != 1 || assistantDTO.Message.ToolCalls[0].Arguments != `{"path":"README.md"}` {
		t.Fatalf("assistant tool DTO = %#v, want filtered display arguments", assistantDTO)
	}
	if assistantDTO.Message.Reasoning != "First I will inspect the repository." {
		t.Fatalf("assistant reasoning DTO = %q, want durable reasoning when enabled", assistantDTO.Message.Reasoning)
	}
	snapshot, err := service.GetSessionSnapshot(session.ID)
	if err != nil {
		t.Fatalf("GetSessionSnapshot(with reasoning) error = %v", err)
	}
	var snapshotAssistant *SessionItem
	for index := range snapshot.History.Items {
		if snapshot.History.Items[index].ID == assistant.ID {
			snapshotAssistant = &snapshot.History.Items[index]
			break
		}
	}
	if snapshotAssistant == nil || snapshotAssistant.Message == nil || snapshotAssistant.Message.Reasoning != "First I will inspect the repository." {
		t.Fatalf("snapshot assistant reasoning = %#v, want persisted reasoning", snapshotAssistant)
	}
	hiddenState, err := service.sessionStore.LoadState(session.ID)
	if err != nil {
		t.Fatalf("LoadState(before hidden reasoning projection) error = %v", err)
	}
	hiddenState.ShowReasoning = false
	if _, err := service.sessionStore.SaveMetadata(hiddenState); err != nil {
		t.Fatalf("SaveMetadata(hide reasoning) error = %v", err)
	}
	hiddenDTO, err := service.sessionItemDTO(session.ID, assistant)
	if err != nil {
		t.Fatalf("sessionItemDTO(hidden reasoning) error = %v", err)
	}
	if hiddenDTO.Message == nil || hiddenDTO.Message.Reasoning != "" {
		t.Fatalf("hidden reasoning DTO = %#v, want omitted when disabled", hiddenDTO.Message)
	}
	if strings.Contains(mustMarshal(t, assistantStreamEvent), "provider-private") {
		t.Fatal("assistant projection event leaked private tool argument")
	}
}

func TestSessionProjectionItemUpdatedCarriesLatestDTOAndSeparatesRevision(t *testing.T) {
	service, _, session := newExecutionServiceWithSession(t, t.TempDir(), nil)
	tool := sessions.SessionItemFromMessage("tool-1", model.Message{
		Role:       model.MessageRoleTool,
		Content:    "initial tool output",
		ToolCallID: "call-1",
	})
	tool.TurnID = "turn-1"
	tool.Status = sessions.ItemStatusPending
	tool, err := service.sessionStore.AppendItem(session.ID, tool)
	if err != nil {
		t.Fatalf("AppendItem(tool) error = %v", err)
	}
	createdSeq := tool.Seq

	updated := tool
	updated.Message = &model.Message{Role: model.MessageRoleTool, Content: "latest tool output", ToolCallID: "call-1", IsError: true}
	updated.Status = sessions.ItemStatusError
	updated, err = service.sessionStore.UpdateItem(session.ID, updated)
	if err != nil {
		t.Fatalf("UpdateItem(tool) error = %v", err)
	}
	if updated.Seq != createdSeq {
		t.Fatalf("updated item seq = %d, want original creation seq %d", updated.Seq, createdSeq)
	}
	state, err := service.sessionStore.LoadState(session.ID)
	if err != nil {
		t.Fatalf("LoadState(after update) error = %v", err)
	}
	updateEvent := persistedItemEvent(t, service, session.ID, sessions.RecordTypeItemUpdated, tool.ID)
	if updateEvent.Seq == createdSeq {
		t.Fatalf("update record seq = %d, want distinct from item creation seq %d", updateEvent.Seq, createdSeq)
	}
	streamEvent, ok := service.sessionStreamEventFromPersistedEvent(session.ID, "run-1", "turn-1", updateEvent, state.LastSeq)
	if !ok {
		t.Fatal("tool update projection event was skipped")
	}
	assertProjectionEnvelope(t, streamEvent, "item.updated", session.ID, "run-1", "turn-1", updateEvent.Seq, state.LastSeq, createdSeq, tool.ID)
	dto := streamEvent["item"].(SessionItem)
	if dto.Seq != createdSeq || dto.Status != sessions.ItemStatusError || dto.Message == nil || dto.Message.Content == nil || dto.Message.Content.Inline != "latest tool output" || !dto.Message.IsError {
		t.Fatalf("updated tool DTO = %#v, want latest committed state with original seq", dto)
	}
}

func TestSessionProjectionItemEventsSkipHiddenAndProviderPrivateItems(t *testing.T) {
	service, _, session := newExecutionServiceWithSession(t, t.TempDir(), nil)
	previousRevision := int64(0)
	items := []sessions.SessionItem{
		sessions.SessionItemFromMessage("hidden-runtime", model.Message{Role: model.MessageRoleDeveloper, Content: "hidden secret"}),
		sessions.SessionItemFromMessage("provider-private", model.Message{Role: model.MessageRoleProvider, Content: "provider secret"}),
	}
	for _, item := range items {
		item.TurnID = "turn-1"
		if _, err := service.sessionStore.AppendItem(session.ID, item); err != nil {
			t.Fatalf("AppendItem(%s) error = %v", item.ID, err)
		}
		event := persistedItemEvent(t, service, session.ID, sessions.RecordTypeItemAppended, item.ID)
		state, err := service.sessionStore.LoadState(session.ID)
		if err != nil {
			t.Fatalf("LoadState(%s) error = %v", item.ID, err)
		}
		if state.LastSeq <= previousRevision {
			t.Fatalf("private item %s did not advance durable revision: before=%d after=%d", item.ID, previousRevision, state.LastSeq)
		}
		previousRevision = state.LastSeq
		if streamEvent, ok := service.sessionStreamEventFromPersistedEvent(session.ID, "run-1", "turn-1", event, state.LastSeq); ok || streamEvent != nil {
			t.Fatalf("private item %s entered projection stream: %#v", item.ID, streamEvent)
		}
	}
}

func TestSessionProjectionCreationUsesCreatedForUserAssistantAndTool(t *testing.T) {
	service, _, session := newExecutionServiceWithSession(t, t.TempDir(), nil)
	items := []sessions.SessionItem{
		sessions.SessionItemFromMessage("user-created", model.Message{Role: model.MessageRoleUser, Content: "hello"}),
		sessions.SessionItemFromMessage("assistant-created", model.Message{Role: model.MessageRoleAssistant, Content: "inspect"}),
		sessions.SessionItemFromMessage("tool-created", model.Message{Role: model.MessageRoleTool, Content: "result", ToolCallID: "call-1"}),
	}
	for index := range items {
		items[index].TurnID = "turn-created"
		if index > 0 {
			items[index].AgentIteration = 1
		}
		if items[index].Message.Role == model.MessageRoleTool {
			items[index].Status = sessions.ItemStatusPending
		}
		var err error
		items[index], err = service.sessionStore.AppendItem(session.ID, items[index])
		if err != nil {
			t.Fatalf("AppendItem(%s) error = %v", items[index].ID, err)
		}
		event := persistedItemEvent(t, service, session.ID, sessions.RecordTypeItemAppended, items[index].ID)
		state, err := service.sessionStore.LoadState(session.ID)
		if err != nil {
			t.Fatalf("LoadState(%s) error = %v", items[index].ID, err)
		}
		streamEvent, ok := service.sessionStreamEventFromPersistedEvent(session.ID, "run-created", "turn-created", event, state.LastSeq)
		if !ok {
			t.Fatalf("%s creation projection was skipped", items[index].ID)
		}
		if streamEvent["type"] != "item.created" || streamEvent["item_id"] != items[index].ID || streamEvent["turn_id"] != "turn-created" || streamEvent["agent_iteration"] != items[index].AgentIteration {
			t.Fatalf("creation projection = %#v, want created and complete identity", streamEvent)
		}
	}

	tool := items[2]
	tool.Message = &model.Message{Role: model.MessageRoleTool, Content: "completed", ToolCallID: "call-1"}
	tool.Status = sessions.ItemStatusCompleted
	updated, err := service.sessionStore.UpdateItem(session.ID, tool)
	if err != nil {
		t.Fatalf("UpdateItem(tool) error = %v", err)
	}
	updateEvent := persistedItemEvent(t, service, session.ID, sessions.RecordTypeItemUpdated, tool.ID)
	state, err := service.sessionStore.LoadState(session.ID)
	if err != nil {
		t.Fatalf("LoadState(after tool update) error = %v", err)
	}
	streamUpdate, ok := service.sessionStreamEventFromPersistedEvent(session.ID, "run-created", "turn-created", updateEvent, state.LastSeq)
	if !ok {
		t.Fatal("tool update projection was skipped")
	}
	updatedDTO := streamUpdate["item"].(SessionItem)
	if streamUpdate["type"] != "item.updated" || streamUpdate["item_id"] != tool.ID || streamUpdate["turn_id"] != "turn-created" || streamUpdate["agent_iteration"] != 1 || updatedDTO.ID != updated.ID || updatedDTO.TurnID != tool.TurnID || updatedDTO.AgentIteration != tool.AgentIteration {
		t.Fatalf("tool update projection = %#v, want same complete identity", streamUpdate)
	}

	for _, event := range []sessions.PersistedEvent{
		persistedItemEvent(t, service, session.ID, sessions.RecordTypeItemAppended, items[0].ID),
		persistedItemEvent(t, service, session.ID, sessions.RecordTypeItemAppended, items[1].ID),
		persistedItemEvent(t, service, session.ID, sessions.RecordTypeItemAppended, items[2].ID),
	} {
		if event.Type != sessions.RecordTypeItemAppended {
			t.Fatalf("unexpected storage event type = %q", event.Type)
		}
	}
}

func TestSessionItemPageConversionUsesOneProvidedStateSnapshot(t *testing.T) {
	service, _, session := newExecutionServiceWithSession(t, t.TempDir(), nil)
	for index, text := range []string{"first", "second"} {
		item := sessions.SessionItemFromMessage("assistant-"+strconv.Itoa(index), model.Message{
			Role:             model.MessageRoleAssistant,
			Content:          text,
			ReasoningContent: "reasoning-" + text,
		})
		item.TurnID = "turn-1"
		item.AgentIteration = index + 1
		if _, err := service.sessionStore.AppendItem(session.ID, item); err != nil {
			t.Fatalf("AppendItem(%d) error = %v", index, err)
		}
	}

	state, err := service.sessionStore.LoadState(session.ID)
	if err != nil {
		t.Fatalf("LoadState(with reasoning) error = %v", err)
	}
	state.ShowReasoning = true
	if _, err := service.sessionStore.SaveMetadata(state); err != nil {
		t.Fatalf("SaveMetadata(show reasoning) error = %v", err)
	}
	state, err = service.sessionStore.LoadState(session.ID)
	if err != nil {
		t.Fatalf("LoadState(snapshot) error = %v", err)
	}
	page, err := service.sessionStore.ReadHistoryPage(session.ID, sessions.HistoryPageOptions{Limit: 10, VisibleOnly: true})
	if err != nil {
		t.Fatalf("ReadHistoryPage() error = %v", err)
	}

	// Change the live metadata after capturing the response state. Every DTO
	// must still use the captured value; a per-item LoadState would make this
	// page lose reasoning halfway through conversion.
	stateAfter := state
	stateAfter.ShowReasoning = false
	if _, err := service.sessionStore.SaveMetadata(stateAfter); err != nil {
		t.Fatalf("SaveMetadata(hide reasoning) error = %v", err)
	}
	converted, err := service.sessionItemsPageFromStorePageWithState(session.ID, page, state)
	if err != nil {
		t.Fatalf("sessionItemsPageFromStorePageWithState() error = %v", err)
	}
	if len(converted.Items) != 2 {
		t.Fatalf("converted item count = %d, want 2", len(converted.Items))
	}
	for _, item := range converted.Items {
		if item.Message == nil || item.Message.Reasoning == "" {
			t.Fatalf("converted item %#v lost reasoning from captured state", item)
		}
	}
}

func persistedItemEvent(t *testing.T, service *Service, sessionID, eventType, itemID string) sessions.PersistedEvent {
	t.Helper()
	events, err := service.sessionStore.PersistedEventsAfter(sessionID, 0)
	if err != nil {
		t.Fatalf("PersistedEventsAfter() error = %v", err)
	}
	for _, event := range events {
		if event.Type == eventType && event.ItemID == itemID {
			return event
		}
	}
	t.Fatalf("persisted %s event for %s not found in %#v", eventType, itemID, events)
	return sessions.PersistedEvent{}
}

func assertProjectionEnvelope(t *testing.T, event SessionStreamEvent, eventType, sessionID, runID, turnID string, recordSeq, revision, itemSeq int64, itemID string) {
	t.Helper()
	if event["type"] != eventType || event["session_id"] != sessionID || event["run_id"] != runID || event["turn_id"] != turnID || event["seq"] != recordSeq || event["revision"] != formatRevision(revision) || event["item_id"] != itemID {
		t.Fatalf("projection envelope = %#v", event)
	}
	item, ok := event["item"].(SessionItem)
	if !ok || item.ID != itemID || item.Seq != itemSeq {
		t.Fatalf("projection item = %#v, want id=%q seq=%d", event["item"], itemID, itemSeq)
	}
}

func formatRevision(seq int64) string {
	return strconv.FormatInt(seq, 10)
}

func mustMarshal(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	return string(data)
}
