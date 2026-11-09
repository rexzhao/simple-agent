package providers

import (
	"simple-agent/internal/locale"

	"context"
	"time"
)

// SimulatedNotice 是没有可用提供商时显示在会话里的引导文案（随界面语言）。
func SimulatedNotice() string { return locale.Tr("providers.simulated.notice") }

// SimulatedProvider 在没有可用凭据时给出一条提示流。
//
// 它让「无凭据」与「真实请求」走**同一条编排流程**（driver 决策 → 事件流），
// 不需要在 UI 里另写一条模拟分支。
type SimulatedProvider struct {
	config ProviderConfig
}

// NewSimulatedProvider 构造模拟提供商。
func NewSimulatedProvider() *SimulatedProvider {
	return &SimulatedProvider{config: SimulatedConfig()}
}

// Config 返回配置。
func (p *SimulatedProvider) Config() ProviderConfig { return p.config }

// Stream 按字符分块输出提示文本，随后结束。
func (p *SimulatedProvider) Stream(ctx context.Context, _ ChatRequest) (<-chan Event, error) {
	out := make(chan Event, 8)

	go func() {
		defer close(out)

		runes := []rune(SimulatedNotice())
		for i := 0; i < len(runes); i += 2 {
			end := min(i+2, len(runes))
			chunk := string(runes[i:end])

			select {
			case out <- Event{Kind: EventText, Text: chunk}:
			case <-ctx.Done():
				return
			}
			select {
			case <-time.After(20 * time.Millisecond):
			case <-ctx.Done():
				return
			}
		}

		select {
		case out <- doneEvent("stop"):
		case <-ctx.Done():
		}
	}()

	return out, nil
}

var _ Provider = (*SimulatedProvider)(nil)
