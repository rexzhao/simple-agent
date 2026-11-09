package host_test

import (
	"testing"

	"simple-agent/internal/host"
	"simple-agent/internal/store"
)

// 旧会话把 locale key 写成了标题：加载时要还原成默认标题。
func TestRestoreTitleFromLocaleKey(t *testing.T) {
	dir := t.TempDir()
	catalog := store.NewCatalog(dir)
	const id = "0123456789abcdef0123456789abcdef"
	log, err := catalog.Create(id)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.NewEvent(store.TypeSessionCreated, map[string]any{
		"context_id": id, "title": "session.defaultTitle", "workspace": dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(created); err != nil {
		t.Fatal(err)
	}
	appended, err := store.NewEvent(store.TypeMessageAppended, map[string]any{
		"message": map[string]any{"messageId": "m1", "role": "ROLE_USER", "parts": []any{}},
		"status":  "complete",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(appended); err != nil {
		t.Fatal(err)
	}
	_ = log.Close()

	local := host.NewLocal(host.Config{Catalog: catalog, Runtime: newP2Runtime(),
		Tr: func(key string, args ...any) string {
			if key == "session.defaultTitle" {
				return "新会话"
			}
			return key
		}})
	t.Cleanup(func() { _ = local.Close() })
	refs := local.Sessions()
	if len(refs) != 1 {
		t.Fatalf("恢复会话数不对：%d", len(refs))
	}
	if refs[0].Title != "新会话" {
		t.Fatalf("标题没有还原：%q", refs[0].Title)
	}
}
