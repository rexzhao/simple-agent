package uiapp_test

import (
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"simple-agent/internal/host"
	"simple-agent/internal/locale"
	"simple-agent/internal/uiapp"
)

// fakeExecutors 记录选中项，并在切换时换掉 App 的 host（模拟 Relay 重连）。
type fakeExecutors struct {
	current string
	hosts   map[string]host.Host
	app     *uiapp.App
	refs    []uiapp.ExecutorRef
	calls   int
}

func (f *fakeExecutors) Executors() ([]uiapp.ExecutorRef, error) {
	f.calls++
	return f.refs, nil
}

func (f *fakeExecutors) Current() string { return f.current }
func (f *fakeExecutors) Switch(hostID string) error {
	f.current = hostID
	f.app.SetHostForTest(f.hosts[hostID])
	f.app.SyncSessionsForTest()
	return nil
}

// 经 relay 时：头部能打开"选择执行端"，选另一台之后草稿不会跟过去，切回来还能恢复。
func TestExecutorPickerSwitchesAndIsolatesDrafts(t *testing.T) {
	app, _, poster := newAppWithOptions(t, func(opts *uiapp.Options) { opts.Language = locale.ZH })
	hostA := app.Host()
	app.SetDraftForTest("A 的草稿")

	// 第二台执行端（独立会话）
	hostB := app.HostForTestNewLocal()
	source := &fakeExecutors{
		current: "exec-a",
		hosts:   map[string]host.Host{"exec-a": hostA, "exec-b": hostB},
		refs: []uiapp.ExecutorRef{
			{HostID: "exec-a", Name: "exec-a", Online: true},
			{HostID: "exec-b", Name: "exec-b", Online: true},
		},
	}
	source.app = app
	app.SetExecutorsForTest(source)

	app.OpenExecutorPicker()
	for i := 0; i < 20 && !app.PickerReadyForTest(); i++ {
		time.Sleep(2 * time.Millisecond)
		poster.Drain()
	}
	if !app.PickerOpenForTest() {
		t.Fatal("没有打开选择执行端列表")
	}
	app.PickExecutorForTest(1) // 选 B
	if app.PickerOpenForTest() {
		t.Fatal("选中之后应当关闭列表")
	}
	if app.DraftForTest() != "" {
		t.Fatalf("A 的草稿不该带到 B：%q", app.DraftForTest())
	}

	app.SetDraftForTest("B 的草稿")
	app.OpenExecutorPicker()
	poster.Drain()
	app.PickExecutorForTest(0) // 切回 A
	if got := app.DraftForTest(); got != "A 的草稿" {
		t.Fatalf("切回 A 应当恢复它的草稿：%q", got)
	}
}

// 渲染 50 帧只能触发很少几次列表拉取（渲染路径绝不能发网络请求）。
func TestExecutorPickerDoesNotFetchPerFrame(t *testing.T) {
	app, _, poster := newAppWithOptions(t, func(opts *uiapp.Options) { opts.Language = locale.ZH })
	source := &fakeExecutors{
		current: "exec-a",
		hosts:   map[string]host.Host{"exec-a": app.Host()},
		refs: []uiapp.ExecutorRef{
			{HostID: "exec-a", Name: "exec-a", Online: true},
			{HostID: "exec-b", Name: "exec-b", Online: true},
		},
	}
	source.app = app
	app.SetExecutorsForTest(source)
	app.OpenExecutorPicker()
	poster.Drain()

	tester := ui.NewTester(app.View, 800, 600)
	for i := 0; i < 50; i++ {
		pump(tester, poster)
	}
	if source.calls > 2 {
		t.Fatalf("渲染 50 帧触发了 %d 次列表拉取（应当 ≤2）", source.calls)
	}
}

// relay 但还没选定执行端（Current 为空）：启动即显示选择列表。
func TestExecutorPickerOpensWhenNoSelection(t *testing.T) {
	app, _, poster := newAppWithOptions(t, func(opts *uiapp.Options) {
		opts.Language = locale.ZH
		opts.Executors = &fakeExecutors{
			current: "",
			hosts:   map[string]host.Host{},
			refs: []uiapp.ExecutorRef{
				{HostID: "exec-a", Name: "exec-a", Online: true},
				{HostID: "exec-b", Name: "exec-b", Online: true},
			},
			app: nil,
		}
	})
	_ = poster
	if !app.PickerOpenForTest() {
		t.Fatal("没有选定执行端时应当直接显示选择列表")
	}
}
