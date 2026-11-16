package driver_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"simple-agent/internal/conversation"
	"simple-agent/internal/driver"
	"simple-agent/internal/providers"
	"simple-agent/internal/store"
	"simple-agent/internal/tools"
)

// 生成一份**新格式**的长会话日志，供分页测试使用。
//
// 背景：/tmp/perf-session.jsonl 是旧格式（message.appended / tool.results），
// 而阶段 3 之后不再兼容旧事件，用它测分页会因为"事件被忽略"而失败。
//
// 用法：SIMPLE_AGENT_WRITE_FIXTURE=/tmp/fixture.jsonl go test -run TestWriteFixture
func TestWriteFixture(t *testing.T) {
	path := os.Getenv("SIMPLE_AGENT_WRITE_FIXTURE")
	if path == "" {
		t.Skip("没有指定 SIMPLE_AGENT_WRITE_FIXTURE")
	}
	const turns = 40

	conv := conversation.New("ctx-fixture")
	d := driver.New(conv, driver.Options{
		Selection: providers.Selection{ProviderID: "fake", Model: "m"},
		Tools:     tools.Builtin(false),
	})
	rec := &recorder{}
	rec.apply(d.Created(""))

	for i := 0; i < turns; i++ {
		effects, err := d.Send(fmt.Sprintf("第 %d 轮：请解释这段代码的行为，以及它在并发下的表现。", i+1))
		if err != nil {
			t.Fatal(err)
		}
		effects = rec.apply(effects)
		start := startStream(t, effects)
		// 一段较长的正文，让每页体积接近真实
		text := "这是一段用于压测的助手回复，包含 **markdown** 加粗、`inline code` 片段，"
		text += "以及一段较长的说明文字用来逼近真实回复的长度。这里再补一些内容让它更长一些。"
		e1, _ := d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventText, Text: text})
		rec.apply(e1)
		e2, _ := d.ApplyEvent(start.MessageID, providers.Event{Kind: providers.EventDone, StopReason: "stop"})
		rec.apply(e2)
	}

	// 写盘：走 store 保证 seq 分配与真实一致。
	// store.Open(dir) 写的是 dir/session.jsonl，所以按目标路径反推目录。
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 目标目录里已有同名文件会冲突，先清掉
	_ = os.Remove(path)
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, event := range rec.events {
		if err := s.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("已写入 %d 条事件到 %s（%d 轮）", len(rec.events), path, turns)
}