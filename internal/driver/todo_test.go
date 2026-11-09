package driver_test

import (
	"testing"

	"simple-agent/internal/conversation"
	"simple-agent/internal/driver"
	"simple-agent/internal/store"
)

// todosOf 收集 effects 里的 todo.write 事件载荷。
func todoEvents(effects []driver.Effect) [][]conversation.TodoItem {
	var out [][]conversation.TodoItem
	for _, effect := range effects {
		record, ok := effect.(driver.Record)
		if !ok || record.Event.Type != store.TypeTodoWrite {
			continue
		}
		var data driver.TodoWriteData
		if err := record.Event.Decode(&data); err != nil {
			continue
		}
		out = append(out, data.Todos)
	}
	return out
}

// SyncTodos 把清单写进会话并记录事件；内容没变时不产生事件。
func TestSyncTodosRecordsOnlyOnChange(t *testing.T) {
	d, _ := newDriver(t, false)

	todos := []conversation.TodoItem{
		{Content: "第一步", Status: conversation.TodoInProgress},
		{Content: "第二步", Status: conversation.TodoPending},
	}
	events := todoEvents(d.SyncTodos(todos))
	if len(events) != 1 || len(events[0]) != 2 {
		t.Fatalf("应记录一条含 2 项的清单事件：%#v", events)
	}
	if got := d.Todos(); len(got) != 2 || got[0].Content != "第一步" {
		t.Fatalf("会话状态没同步：%#v", got)
	}

	// 同样的内容再来一次：不写事件，避免每个工具轮都刷一条无变化事件
	if again := todoEvents(d.SyncTodos(todos)); len(again) != 0 {
		t.Fatalf("内容未变不应记录事件：%#v", again)
	}
}

// 用户发新消息时清空上一轮的清单（对齐 dsh 的 turn/start），
// 且清空本身也落一条事件（否则重放会保留旧清单）。
func TestSendClearsTodos(t *testing.T) {
	d, _ := newDriver(t, false)
	d.SyncTodos([]conversation.TodoItem{
		{Content: "上一轮的任务", Status: conversation.TodoInProgress},
	})

	effects, err := d.Send("新的一轮")
	if err != nil {
		t.Fatalf("发送应成功：%v", err)
	}
	if got := d.Todos(); len(got) != 0 {
		t.Fatalf("新消息应清空清单，实际 %#v", got)
	}
	// 清空要留下事件：最后一条 todo.write 应是空清单
	events := todoEvents(effects)
	if len(events) == 0 {
		t.Fatal("清空清单也要记录事件")
	}
	if len(events[len(events)-1]) != 0 {
		t.Fatalf("最后一条清单事件应是空的：%#v", events)
	}
}

// 插话（steer）是同一轮的后续输入，**不该**清空正在用的清单。
func TestSteerKeepsTodos(t *testing.T) {
	d, _ := newDriver(t, false)
	effects, err := d.Send("开始")
	if err != nil {
		t.Fatalf("发送失败：%v", err)
	}
	_ = startStream(t, effects)
	d.SyncTodos([]conversation.TodoItem{
		{Content: "进行中的任务", Status: conversation.TodoInProgress},
	})

	if _, err := d.Enqueue("顺便说一句"); err != nil {
		t.Fatalf("插话应成功：%v", err)
	}
	if got := d.Todos(); len(got) != 1 {
		t.Fatalf("插话期间清单应保留，实际 %#v", got)
	}
}

// 清单要能被重放重建：这是它必须落盘的全部理由。
func TestTodosSurviveReplay(t *testing.T) {
	d, conv := newDriver(t, false)
	events := []store.Event{store.MustEvent(store.TypeSessionCreated, driver.SessionCreatedData{
		ContextID: conv.ContextID(), Title: "t",
	})}

	// 两次写入：后一次整体替换前一次（last-write-wins）
	for _, todos := range [][]conversation.TodoItem{
		{
			{Content: "旧的", Status: conversation.TodoInProgress},
		},
		{
			{Content: "第一步", Status: conversation.TodoCompleted},
			{Content: "第二步", Status: conversation.TodoInProgress},
		},
	} {
		for _, effect := range d.SyncTodos(todos) {
			if record, ok := effect.(driver.Record); ok {
				events = append(events, record.Event)
			}
		}
	}

	replayed, err := driver.Replay(store.Renumber(events))
	if err != nil {
		t.Fatalf("重放失败：%v", err)
	}
	todos := replayed.Conversation.Todos()
	if len(todos) != 2 {
		t.Fatalf("重放应得到最后一次的 2 项，实际 %#v", todos)
	}
	if todos[0].Content != "第一步" || todos[0].Status != conversation.TodoCompleted {
		t.Fatalf("清单内容不一致：%#v", todos)
	}
	if todos[1].Status != conversation.TodoInProgress {
		t.Fatalf("清单状态不一致：%#v", todos)
	}
}

// 空清单事件（清空）要能被重放成「没有清单」，而不是保留上一份。
func TestTodoClearSurvivesReplay(t *testing.T) {
	_, conv := newDriver(t, false)
	events := []store.Event{
		store.MustEvent(store.TypeTodoWrite, driver.TodoWriteData{Todos: []conversation.TodoItem{
			{Content: "会被清掉", Status: conversation.TodoPending},
		}}),
		store.MustEvent(store.TypeTodoWrite, driver.TodoWriteData{}),
	}
	replayed, err := driver.Replay(store.Renumber(events))
	if err != nil {
		t.Fatalf("重放失败：%v", err)
	}
	if got := replayed.Conversation.Todos(); len(got) != 0 {
		t.Fatalf("清空事件后不应有清单：%#v", got)
	}
	_ = conv
}

// conversation 层的清单语义：整体替换、返回副本、空即清空。
func TestConversationTodoSemantics(t *testing.T) {
	conv := conversation.New("c")
	if conv.Todos() != nil {
		t.Fatal("初始应为空清单")
	}
	conv.SetTodos([]conversation.TodoItem{{Content: "a", Status: conversation.TodoPending}})
	got := conv.Todos()
	got[0].Content = "改副本"
	if conv.Todos()[0].Content != "a" {
		t.Fatal("Todos 应返回副本，改它不能影响会话")
	}
	conv.SetTodos(nil)
	if conv.Todos() != nil {
		t.Fatal("传 nil 应清空")
	}
}
