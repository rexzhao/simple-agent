package host_test

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"simple-agent/internal/host"
	"simple-agent/internal/providers"
	"simple-agent/internal/wire"
)

// --net-log：收到/发出每个包都打一行（kind + 字节数）。
func TestNetLogRecordsPackets(t *testing.T) {
	var mu sync.Mutex
	var buf bytes.Buffer
	host.SetNetLogOutput(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}))
	t.Cleanup(func() { host.SetNetLogOutput(nil) })
	text := func() string { mu.Lock(); defer mu.Unlock(); return buf.String() }

	host.EnableNetLog(true)
	t.Cleanup(func() { host.EnableNetLog(false) })

	pair := newP2Pair(t, true)
	pair.pumpUntil(t, func() bool { return pair.remote.AttachCountForTest(pair.sessID) >= 1 })

	payload, _ := host.EncodePayload(host.SendPayload{Text: "记录一下"})
	if _, err := pair.remote.Submit(host.Command{SessionID: pair.sessID, Kind: "send", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		pair.post.pump()
		if strings.Contains(text(), "kind=cmd") && strings.Contains(text(), "kind=cmd.result") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	out := text()
	if !strings.Contains(out, "net ⇐") || !strings.Contains(out, "net ⇒") {
		t.Fatalf("没有收发日志：%s", out)
	}
	if !strings.Contains(out, "kind=cmd") {
		t.Fatalf("没有命令包的日志：%s", out)
	}
	_ = wire.Packet{}
	_ = providers.Selection{}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
