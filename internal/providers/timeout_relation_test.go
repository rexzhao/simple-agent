package providers

import (
	"net/http"
	"testing"
	"time"
)

// 首字节超时必须**明显宽于**内容超时。
//
// 这是修"卡 95 秒后失败、out=0"的核心：
//   - contentTimeout 原本从请求发出就开始计时，首字节前的等待
//     （服务端排队 + prefill 大上下文，实测要 41 秒）也被算成"没有输出"
//   - 大上下文会话稍慢就撞上 90 秒被自己掐断，看着像网关挂了
//
// 所以首字节之前用 firstEventTimeout（宽松），收到第一个事件后才切到
// contentTimeout（严格）。两者关系一旦反过来，这个 bug 就回来了。
func TestFirstEventTimeoutIsMoreLenient(t *testing.T) {
	if firstEventTimeout <= contentTimeout {
		t.Fatalf("首字节超时（%s）必须大于内容超时（%s），否则慢首字节会被误杀",
			firstEventTimeout, contentTimeout)
	}
	// 留出足够余量：实测首字节 41 秒，网关波动一倍也得撑住。
	if firstEventTimeout < 3*contentTimeout {
		t.Fatalf("首字节超时（%s）相对内容超时（%s）余量太小",
			firstEventTimeout, contentTimeout)
	}
}

// 响应头超时不能先于我们自己的 watchdog 触发。
//
// 否则用户看到的错误来自 net/http（"timeout awaiting response headers"），
// 既没有人话解释，也分不清"首字节慢"和"真卡住"。
func TestResponseHeaderTimeoutOutlastsFirstEvent(t *testing.T) {
	transport, ok := sharedClient.Transport.(*http.Transport)
	if !ok {
		t.Skip("Transport 不是 http.Transport，跳过")
	}
	if transport.ResponseHeaderTimeout <= firstEventTimeout {
		t.Fatalf("响应头超时（%s）必须大于首字节超时（%s），否则 watchdog 永远轮不到",
			transport.ResponseHeaderTimeout, firstEventTimeout)
	}
	_ = time.Second
}
