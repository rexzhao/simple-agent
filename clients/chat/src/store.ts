// The session store: one live session's record stream, chain, queue,
// and in-flight item — the same state machine the log page runs
// (internal/web/static/reducer.mjs), mirrored for the chat view. The
// chain module is imported from the server's static tree so the fold
// stays single-source.
import { chainGenesis, chainStep } from "../../../internal/web/static/chain.mjs";
import type { Frame, Record, SteeredWire } from "./types";

export type LivePart =
  | { type: "text"; text: string }
  | { type: "reasoning"; text: string }
  | { type: "tool-call"; toolCallId: string; toolName: string; argsText: string; result?: string; isError?: boolean; running: boolean };

export type LiveItem = {
  id: string;
  parts: LivePart[];
  failed: boolean;
};

export type Snap = {
  version: number;
  session: string;
  records: readonly Record[];
  live: LiveItem | null;
  steered: readonly SteeredWire[];
  running: boolean;
  compacting: boolean;
  more: boolean;
  prefixAnchor: string;
  head: string;
  connected: boolean;
  verified: boolean | null; // null = not yet comparable
  notice: string;
};

const initialSnap: Snap = {
  version: 0,
  session: "",
  records: [],
  live: null,
  steered: [],
  running: false,
  compacting: false,
  more: false,
  prefixAnchor: "",
  head: "",
  connected: false,
  verified: null,
  notice: "",
};

export class SessionStore {
  snap: Snap = initialSnap;
  private listeners = new Set<() => void>();
  private es: EventSource | null = null;
  private closed = false;

  subscribe = (cb: () => void) => {
    this.listeners.add(cb);
    return () => this.listeners.delete(cb);
  };

  getSnapshot = () => this.snap;

  private commit(patch: Partial<Snap>) {
    this.snap = { ...this.snap, ...patch, version: this.snap.version + 1 };
    for (const cb of this.listeners) cb();
  }

  // open attaches this store to a session: a fresh store connects with
  // a full snapshot; a cached one resumes from its held head.
  async open(id: string) {
    if (this.closed || this.snap.session === id && this.es) return;
    if (this.snap.session !== id) {
      this.snap = { ...initialSnap, session: id, head: await chainGenesis(id), prefixAnchor: await chainGenesis(id) };
    }
    this.connect();
  }

  private connect() {
    const from = this.snap.head && this.snap.records.length ? this.snap.head : "";
    const url = `/api/s/${this.snap.session}/stream` + (from ? `?from=${encodeURIComponent(from)}` : "");
    const es = new EventSource(url);
    this.es = es;
    es.onmessage = (ev) => {
      let frame: Frame;
      try {
        frame = JSON.parse(ev.data);
      } catch {
        return;
      }
      void this.apply(frame);
    };
    es.onerror = () => {
      // EventSource auto-reconnects; the reconnect replays from our
      // held head (resume) or the window (miss), same as the log page.
      this.commit({ connected: false });
    };
  }

  // detach stops the stream but keeps the folded state — switching back
  // resumes from the head. The server releases its refs when the
  // connection drops (the lazy-open/evict cycle).
  detach() {
    this.es?.close();
    this.es = null;
    this.commit({ connected: false });
  }

  destroy() {
    this.closed = true;
    this.detach();
  }

  private async apply(frame: Frame) {
    switch (frame.kind) {
      case "snapshot": {
        let records: Record[];
        let head: string;
        let prefixAnchor: string;
        if (frame.resumed) {
          records = [...this.snap.records];
          head = this.snap.head;
          prefixAnchor = this.snap.prefixAnchor;
        } else {
          records = [];
          head = frame.anchor || "";
          prefixAnchor = frame.anchor || "";
        }
        for (const r of frame.records || []) {
          records.push(r);
          head = await chainStep(head, r);
        }
        const live = liveFromSnapshot(frame);
        this.commit({
          records,
          head,
          prefixAnchor,
          more: !!frame.more,
          running: !!frame.running,
          live,
          steered: frame.steered || [],
          connected: true,
          verified: frame.d ? frame.d === head : this.snap.verified,
        });
        break;
      }
      case "record": {
        if (!frame.r) break;
        const records = [...this.snap.records, frame.r];
        const head = await chainStep(this.snap.head, frame.r);
        // The committed record supersedes the live item by ItemID.
        const live = this.snap.live && this.snap.live.id !== frame.r.item ? this.snap.live : dropLiveIfEmpty(this.snap.live, frame.r);
        this.commit({ records, head, live, verified: null });
        break;
      }
      case "event": {
        this.applyEvent(frame);
        break;
      }
      case "status": {
        this.commit({
          running: frame.running ?? this.snap.running,
          compacting: !!frame.compacting,
          notice: frame.notice || "",
          verified: frame.d ? frame.d === this.snap.head : this.snap.verified,
        });
        break;
      }
    }
  }

  private applyEvent(frame: Frame) {
    switch (frame.t) {
      case "assistant_message_started": {
        if (!frame.item) break;
        if (this.snap.live && this.snap.live.id === frame.item) break;
        this.commit({ live: { id: frame.item, parts: [], failed: false } });
        break;
      }
      case "reasoning_delta":
      case "text_delta": {
        const live = this.snap.live;
        if (!live || (frame.item && frame.item !== live.id)) break;
        const kind = frame.t === "text_delta" ? "text" : "reasoning";
        const parts = appendText(live.parts, kind, frame.text || "");
        this.commit({ live: { ...live, parts } });
        break;
      }
      case "tool_started":
      case "tool_call_done": {
        const live = this.snap.live;
        if (!live || !frame.call_id) break;
        const parts = upsertTool(live.parts, frame.call_id, frame.name || "", frame.args || "", frame.t === "tool_call_done");
        this.commit({ live: { ...live, parts } });
        break;
      }
      case "tool_result": {
        const live = this.snap.live;
        if (!live || !frame.call_id) break;
        const parts = attachResult(live.parts, frame.call_id, frame.text || "", !!frame.is_err);
        this.commit({ live: { ...live, parts } });
        break;
      }
      case "assistant_message_completed": {
        const live = this.snap.live;
        if (!live || (frame.item && frame.item !== live.id)) break;
        // The record lands right behind this event and replaces the
        // live item; keep it until then.
        break;
      }
      case "assistant_message_failed": {
        const live = this.snap.live;
        if (!live || (frame.item && frame.item !== live.id)) break;
        this.commit({ live: { ...live, failed: true } });
        break;
      }
      case "steer_queued": {
        if (!frame.item) break;
        const steered = [...this.snap.steered.filter(s => s.item !== frame.item), { item: frame.item, text: frame.text || "" }];
        this.commit({ steered });
        break;
      }
      case "steer_updated": {
        if (!frame.item) break;
        const steered = this.snap.steered.map(s => (s.item === frame.item ? { ...s, text: frame.text || "" } : s));
        this.commit({ steered });
        break;
      }
      case "steer_withdrawn": {
        if (!frame.item) break;
        this.commit({ steered: this.snap.steered.filter(s => s.item !== frame.item) });
        break;
      }
      default:
        break;
    }
  }

  // loadEarlier pages history backward and verifies the batch the same
  // strong way the log page does: fold(batch) must equal the prefix
  // anchor, or the page is dropped as drift.
  async loadEarlier(): Promise<boolean> {
    const { session, prefixAnchor } = this.snap;
    if (!session || !prefixAnchor) return false;
    const { getHistory } = await import("./api");
    let data;
    try {
      data = await getHistory(session, prefixAnchor);
    } catch {
      return false;
    }
    let h = data.anchor;
    for (const r of data.records) h = await chainStep(h, r);
    if (h !== this.snap.prefixAnchor) return false;
    this.commit({
      records: [...data.records, ...this.snap.records],
      prefixAnchor: data.anchor,
      more: !!data.more,
    });
    return true;
  }
}

// ---- live item helpers ----

function liveFromSnapshot(frame: Frame): LiveItem | null {
  if (!frame.streaming) return null;
  const parts: LivePart[] = [];
  if (frame.streaming.reasoning) parts.push({ type: "reasoning", text: frame.streaming.reasoning });
  if (frame.streaming.content) parts.push({ type: "text", text: frame.streaming.content });
  if (frame.tool && frame.tool.name) {
    parts.push({ type: "tool-call", toolCallId: frame.tool.item, toolName: frame.tool.name, argsText: frame.tool.args || "", running: true });
  }
  return { id: frame.streaming.item, parts, failed: false };
}

// A committed record that matches the live item's id may carry LESS
// than the deltas accumulated (they fold the same content in practice)
// — either way the record is the truth: drop the live item.
function dropLiveIfEmpty(live: LiveItem | null, r: Record): LiveItem | null {
  if (!live) return null;
  if (r.item === live.id) return null;
  return live;
}

function appendText(parts: LivePart[], kind: "text" | "reasoning", text: string): LivePart[] {
  const last = parts[parts.length - 1];
  if (last && last.type === kind) {
    return [...parts.slice(0, -1), { type: kind, text: last.text + text }];
  }
  return [...parts, { type: kind, text }];
}

function upsertTool(parts: LivePart[], callID: string, name: string, args: string, done: boolean): LivePart[] {
  const i = parts.findIndex(p => p.type === "tool-call" && p.toolCallId === callID);
  if (i === -1) return [...parts, { type: "tool-call", toolCallId: callID, toolName: name, argsText: args, running: !done }];
  const part = parts[i];
  if (part.type !== "tool-call") return parts;
  const next = [...parts];
  next[i] = { ...part, toolName: name || part.toolName, argsText: args || part.argsText, running: !done };
  return next;
}

function attachResult(parts: LivePart[], callID: string, result: string, isError: boolean): LivePart[] {
  const i = parts.findIndex(p => p.type === "tool-call" && p.toolCallId === callID);
  if (i === -1) return parts;
  const part = parts[i];
  if (part.type !== "tool-call") return parts;
  const next = [...parts];
  next[i] = { ...part, result, isError, running: false };
  return next;
}
