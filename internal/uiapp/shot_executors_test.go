package uiapp_test

import (
	"image/png"
	"os"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"simple-agent/internal/host"
	"simple-agent/internal/locale"
	"simple-agent/internal/uiapp"
)

// 执行端选择列表的截图（SIMPLE_AGENT_SHOT=/tmp/executors.png 时写出）。
func TestExecutorPickerScreenshot(t *testing.T) {
	app, _, p := newAppWithOptions(t, func(opts *uiapp.Options) { opts.Language = locale.ZH })
	source := &fakeExecutors{
		current: "exec-a",
		hosts:   map[string]host.Host{"exec-a": app.Host()},
		refs: []uiapp.ExecutorRef{
			{HostID: "exec-a", Name: "designer-mac", Online: true},
			{HostID: "exec-b", Name: "ci-runner", Online: false},
		},
	}
	source.app = app
	app.SetExecutorsForTest(source)
	app.OpenExecutorPicker()
	for i := 0; i < 50 && !app.PickerReadyForTest(); i++ {
		time.Sleep(2 * time.Millisecond)
		p.Drain()
	}

	tester := ui.NewTester(app.View, 900, 620)
	pump(tester, p)
	pump(tester, p)

	if path := os.Getenv("SIMPLE_AGENT_SHOT"); path != "" {
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if err := png.Encode(file, tester.Image()); err != nil {
			t.Fatal(err)
		}
		t.Logf("已写出 %s", path)
	}
	for _, want := range []string{"选择执行端", "designer-mac", "ci-runner", "离线"} {
		if !tester.HasText(want) {
			t.Fatalf("列表里缺少 %q：%v", want, tester.Texts())
		}
	}
}
