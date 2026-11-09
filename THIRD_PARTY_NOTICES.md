# 第三方声明

本项目的部分**视觉设计令牌（颜色、字号、间距、字体栈）、中文文案与图标**取自
[deepseek-harness](https://github.com/deepseek-ai)（本地参考副本：`F:/work/deepseek-harness`），
按 MIT 许可证使用。

对应文件与用途：

| 本项目 | 取自 deepseek-harness | 用途 |
|---|---|---|
| `internal/uiapp/theme_dsh.go` | `packages/client/ui-theme/src/styles/design-platform.css`（浅色段）、`base.css` | 浅色主题的色值与字体栈 |
| `internal/uiapp/layout.go` | `packages/client/ui-layout/src/client/columns.ts` | 侧栏 / 正文列尺寸 |
| `internal/uiapp/process_activity.go` | `packages/client/ui-chat/src/client/locale.ts`、`contract/process-groups.ts` | 过程组分类与文案 |
| `internal/uiapp/icons.go`（后续加入） | `packages/client/ui-primitives/src/icons/index.tsx` | 界面图标 |

---

MIT License

Copyright (c) 2026 DeepSeek

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
