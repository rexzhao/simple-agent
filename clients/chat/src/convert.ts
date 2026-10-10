// Records → ThreadMessageLike. The JOIN the analysis predicted: our
// wire is a flat record stream (tool.result and run.failed are their
// own records with their own ItemIDs), while assistant-ui nests them
// into the parent assistant message's parts and status. The merge index
// is keyed by tool_call_id; a result with no parent in the window is
// dropped (the log view at /log keeps record-level truth).
import type { ThreadMessageLike } from "@assistant-ui/react";
import type { LiveItem, LivePart } from "./store";
import type { Record } from "./types";

type Part =
  | { type: "text"; text: string }
  | { type: "reasoning"; text: string }
  | {
      type: "tool-call";
      toolCallId: string;
      toolName: string;
      argsText: string;
      result?: string;
      isError?: boolean;
    };

type Status =
  | { type: "running" }
  | { type: "complete"; reason: "stop" | "unknown" }
  | { type: "incomplete"; reason: "cancelled" | "error" | "other"; error?: string };

export type ChatMessage = {
  id: string;
  role: "user" | "assistant";
  parts: Part[];
  status: Status;
  createdAt?: Date;
  // The run (user input through final response) a record belongs to —
  // run scoping keeps failures and tool results from folding onto a
  // PREVIOUS run's completed messages.
  run?: string;
};

export function convertRecords(records: readonly Record[]): ChatMessage[] {
  const out: ChatMessage[] = [];
  for (const r of records) {
    switch (r.t) {
      case "user.input":
        out.push({ id: r.item || r.ts, role: "user", parts: [{ type: "text", text: r.content || "" }], status: { type: "complete", reason: "stop" as const }, run: r.run });
        break;
      case "assistant.message": {
        const parts: Part[] = [];
        if (r.reasoning) parts.push({ type: "reasoning", text: r.reasoning });
        if (r.content) parts.push({ type: "text", text: r.content });
        for (const call of r.tool_calls || []) {
          parts.push({ type: "tool-call", toolCallId: call.id, toolName: call.name, argsText: call.arguments });
        }
        out.push({ id: r.item || r.ts, role: "assistant", parts, status: { type: "complete", reason: "stop" as const }, run: r.run });
        break;
      }
      case "tool.result": {
        // Fold into the newest assistant message OF THE SAME RUN carrying
        // this call. The run guard matters: the join scans backward and
        // must not cross a run boundary onto the previous round's
        // message (safe today by write order — results land between
        // their assistant and the next user input — but the guard makes
        // it structural).
        for (let i = out.length - 1; i >= 0; i--) {
          const m = out[i];
          if (m.run !== r.run) continue;
          if (m.role !== "assistant") continue;
          const part = m.parts.find(p => p.type === "tool-call" && p.toolCallId === r.tool_call_id);
          if (part && part.type === "tool-call") {
            part.result = r.content || "";
            part.isError = !!r.is_error;
            break;
          }
        }
        break;
      }
      case "run.failed": {
        // Fold onto the latest message OF THE SAME RUN; a failure of run
        // N+1 must not re-mark run N's completed answer. No same-run
        // message in the window: standalone error row.
        let target: ChatMessage | undefined;
        for (let i = out.length - 1; i >= 0; i--) {
          if (out[i].run === r.run) {
            target = out[i];
            break;
          }
        }
        if (target && target.role === "assistant") {
          target.status = { type: "incomplete", reason: "error", error: r.error };
        } else {
          out.push({ id: r.item || r.ts, role: "assistant", parts: [], status: { type: "incomplete", reason: "error", error: r.error }, run: r.run });
        }
        break;
      }
      default:
        // session.created / context.compacted: metadata records, no
        // chat rendering (the log view shows them).
        break;
    }
  }
  return out;
}

export function convertLive(live: LiveItem | null): ChatMessage | null {
  if (!live) return null;
  return {
    id: live.id,
    role: "assistant",
    parts: live.parts.map(partFromLive),
    status: live.failed ? { type: "incomplete", reason: "error" } : { type: "running" },
  };
}

function partFromLive(p: LivePart): Part {
  if (p.type === "text" || p.type === "reasoning") return { type: p.type, text: p.text };
  return { type: "tool-call", toolCallId: p.toolCallId, toolName: p.toolName, argsText: p.argsText, result: p.result, isError: p.isError };
}

// The converter handed to useExternalStoreRuntime: our ChatMessage is
// already the ThreadMessageLike shape modulo status normalization.
export function toThreadMessageLike(m: ChatMessage, _idx: number): ThreadMessageLike {
  const parts = m.parts.map(p => {
    if (p.type === "text") return { type: "text" as const, text: p.text };
    if (p.type === "reasoning") return { type: "reasoning" as const, text: p.text };
    return {
      type: "tool-call" as const,
      toolCallId: p.toolCallId,
      toolName: p.toolName,
      argsText: p.argsText,
      result: p.result,
      isError: p.isError,
    };
  });
  return {
    role: m.role,
    content: parts,
    id: m.id,
    createdAt: m.createdAt,
    status: m.status,
  };
}
