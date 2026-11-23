package uiapp

import (
	"testing"

	"simple-agent/internal/host"
)

// 启动时选中"上次打开的那个会话"。
//
// 用户报的："桌面版没有记录用户最后打开的会话，每次打开时候默认会话不确定"。
func TestPickActivePrefersLastSession(t *testing.T) {
	// Session 内嵌了 host.Session 指针，ID 走嵌字段 —— 不能造 nil 的壳。
	// 用最小可用的壳：只要 ID 对得上就行，选择逻辑不碰别的字段。
	newShell := func(id string) *Session {
		return &Session{Session: &host.Session{ID: id}}
	}
	sessions := []*Session{
		newShell("a"),
		newShell("b"),
		newShell("c"),
	}

	cases := []struct {
		name              string
		previous, last, h string
		want              int
	}{
		{
			name: "首次启动：用上次打开的",
			last: "b", h: "a",
			want: 1,
		},
		{
			name: "启动时没有记录：退化成执行端的当前会话",
			h:    "c",
			want: 2,
		},
		{
			name: "记录指向已删除的会话：退化成执行端的当前会话",
			last: "gone", h: "a",
			want: 0,
		},
		{
			name:     "运行中刷新列表：保持当前会话（不被 last 拉走）",
			previous: "c", last: "b", h: "a",
			want: 2,
		},
		{
			name:     "当前会话被删了：退化成执行端的当前会话",
			previous: "gone", last: "b", h: "a",
			// previous 找不到后**不会**去用 last（那是启动逻辑），
			// 直接落到执行端的当前会话 —— 否则删除后会被莫名拉走。
			want: 0,
		},
		{
			name: "什么都没有：落到最后一个",
			want: 2,
		},
	}
	for _, c := range cases {
		got := pickActiveSession(sessions, c.previous, c.last, c.h)
		if got != c.want {
			t.Fatalf("%s：期望 %d，实际 %d", c.name, c.want, got)
		}
	}
}

// 没有任何会话时返回 -1（调用方会兜成 0，不会越界）。
func TestPickActiveEmpty(t *testing.T) {
	if got := pickActiveSession(nil, "", "x", "y"); got != -1 {
		t.Fatalf("空列表应返回 -1，实际 %d", got)
	}
}
