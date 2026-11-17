#!/usr/bin/env python3
"""生成一份长会话日志，供 UI 的滚动/分页测试使用。

为什么不用 Go 写：只是想造测试数据，用 Python 拼 JSON 更直接；
生成结果会被执行端当作真实会话读取（格式与 driver/record.go 一致）。

用法：
    python3 scripts/make-test-session.py <输出目录> [消息对数]

产出：
    <输出目录>/events.jsonl
"""
import json
import os
import sys
import uuid
from datetime import datetime, timedelta, timezone


def main(argv):
    if len(argv) < 2:
        print(__doc__)
        return 2
    out_dir = argv[1]
    pairs = int(argv[2]) if len(argv) > 2 else 60  # 每条 user+agent 算一对

    os.makedirs(out_dir, exist_ok=True)
    path = os.path.join(out_dir, "events.jsonl")

    seq = 0
    now = datetime.now(timezone.utc)
    context_id = str(uuid.uuid4())

    def emit(kind, data):
        nonlocal seq
        seq += 1
        rec = {
            "v": 1,
            "seq": seq,
            "at": (now + timedelta(seconds=seq)).isoformat().replace("+00:00", "Z"),
            "type": kind,
            "data": data,
        }
        return json.dumps(rec, ensure_ascii=False)

    lines = []
    lines.append(emit("session.created", {
        "context_id": context_id,
        "title": f"滚动测试会话（{pairs} 轮）",
        "created_at": now.isoformat().replace("+00:00", "Z"),
        "workspace": "/tmp/sa-test",
    }))

    round_no = 0
    for i in range(pairs):
        round_no += 1
        # 用户输入
        lines.append(emit("message.input", {
            "round": round_no,
            "turn": 1,
            "role": "user",
            "message": {
                "messageId": f"u{i}",
                "role": "user",
                "parts": [{
                    "kind": "text",
                    "text": f"第 {i + 1} 个问题：" +
                            ("这是一段用来占位的中等长度提问文本。" * (1 + i % 3)),
                }],
            },
            "status": "complete",
        }))
        # agent 回答：正文长短交替，制造不等高的条目 —— 这样才能验证
        # "滚动锚定保住了条目内偏移"，而不是碰巧对齐到条目顶部。
        body = (
            f"**第 {i + 1} 个回答**\n\n"
            + ("这是一段回答正文，用来制造足够的视觉高度。" * (6 if i % 5 == 0 else 2))
        )
        lines.append(emit("message.output", {
            "round": round_no,
            "turn": 1,
            "role": "agent",
            "message": {
                "messageId": f"a{i}",
                "role": "agent",
                "parts": [{"kind": "text", "text": body}],
            },
            "status": "complete",
        }))

    with open(path, "w", encoding="utf-8", newline="\n") as f:
        f.write("\n".join(lines) + "\n")

    print(f"已生成 {path}")
    print(f"  事件数: {len(lines)}")
    print(f"  消息对: {pairs}")
    print(f"  大小: {os.path.getsize(path) / 1024:.1f} KB")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
