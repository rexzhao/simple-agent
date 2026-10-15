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
  // The single in-flight tool execution (the server has one Running
  // slot). It overlays the matching tool-call part of the last
  // assistant message with its executing state.
  tool: { callID: string; name: string; args: string; awaitingApproval: boolean } | null;
  // The approval card (§16.3): pending while the tool slot blocks on
  // the host's decision, outcome set by the resolved event until the
  // tool completes. item is the future tool.result's identity — the
  // same id POST /approval takes.
  approval: {
    item: string;
    name: string;
    args: string;
    outcome: string | null;
  } | null;
};

const initialSnap: Snap = {
  approval: null,
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
  tool: null,
};

export type EventSourceLike = {
  onmessage: ((ev: { data: string }) => void) | null;
  onerror: (() => void) | null;
  close(): void;
};

export class SessionStore {
  snap: Snap = initialSnap;
  private listeners = new Set<() => void>();
  private es: EventSourceLike | null = null;
  private closed = false;
  // Bumped on detach: an open() awaiting its genesis must not connect
  // a store the view has already left.
  private epoch = 0;
  // Serialized frame processing (see connect).
  private frameQueue: Promise<void> = Promise.resolve();
  private esFactory: (url: string) => EventSourceLike;

  constructor(esFactory?: (url: string) => EventSourceLike) {
    this.esFactory =
      esFactory ?? ((url: string) => new EventSource(url) as unknown as EventSourceLike);
  }

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
    if (!id || this.closed || (this.snap.session === id && this.es)) return;
    const epoch = this.epoch;
    if (this.snap.session !== id) {
      const genesis = await chainGenesis(id);
      // Detached while awaiting (the view switched away): connecting
      // now would open a stream for a session nobody is looking at —
      // and the stream HOLDS server-side refs.
      if (this.epoch !== epoch || this.closed) return;
      this.snap = { ...initialSnap, session: id, head: genesis, prefixAnchor: genesis };
    }
    this.connect();
  }

  private connect() {
    if (!this.snap.session) return;
    const from = this.snap.head && this.snap.records.length ? this.snap.head : "";
    const url = `/api/s/${this.snap.session}/stream` + (from ? `?from=${encodeURIComponent(from)}` : "");
    const es = this.esFactory(url);
    this.es = es;
    // Frames are SERIALIZED through a promise queue: apply awaits the
    // digest fold (async crypto), and two frames arriving in one tick
    // would otherwise read the same stale snapshot — the later commit
    // silently dropping the earlier record and skewing the chain. The
    // log page serializes the same way (its enqueue); status frames
    // queuing behind record frames also fixes premature drift reports.
    es.onmessage = (ev) => {
      let frame: Frame;
      try {
        frame = JSON.parse(ev.data);
      } catch {
        return;
      }
      this.frameQueue = this.frameQueue.then(() => this.apply(frame)).catch(() => {
        /* a bad frame must not kill the chain */
      });
    };
    es.onerror = () => {
      // EventSource auto-reconnects; the reconnect replays from our
      // held head (resume) or the window (miss), same as the log page.
      this.commit({ connected: false });
    };
  }

  // collapseApproval drops the card locally: the approval is gone
  // (the POST answered approval_unknown — handled or never existed)
  // and no resolved event is coming to clear it.
  collapseApproval() {
    if (this.snap.approval) this.commit({ approval: null });
  }

  // detach stops the stream but keeps the folded state — switching back
  // resumes from the head. The server releases its refs when the
  // connection drops (the lazy-open/evict cycle).
  detach() {
    this.epoch++;
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
        // A resumed snapshot whose anchor is NOT our head means the
        // EventSource reconnected with its original URL (the from
        // digest is frozen at connect time) after we already folded
        // newer records — the replayed suffix overlaps what we hold.
        // Rebuild from the anchor position instead of appending:
        // locate the anchor in our held chain by folding, keep the
        // records before it (history pages included), and take the
        // server's suffix as the truth from there.
        const overlap = frame.resumed && frame.anchor !== this.snap.head;
        let records: Record[] = [];
        let head: string;
        let prefixAnchor: string;
        if (!frame.resumed) {
          // Window reissue: clean rebuild from the server-attested
          // anchor — never genesis (a running hash converges from any
          // correct anchor; genesis is only correct when nothing was
          // truncated).
          prefixAnchor = frame.anchor || "";
          head = prefixAnchor;
        } else if (overlap) {
          // Locate the anchor in our held chain by folding: everything
          // folded BEFORE it (history pages included) is kept, and the
          // server's suffix is folded from the anchor. Anchor not
          // found (should not happen — we sent this digest) degrades
          // to the window-reissue rebuild.
          prefixAnchor = this.snap.prefixAnchor;
          let h = this.snap.prefixAnchor;
          let k = 0;
          for (; k < this.snap.records.length; k++) {
            if (h === frame.anchor) break;
            h = await chainStep(h, this.snap.records[k]);
          }
          if (h === frame.anchor) {
            records = this.snap.records.slice(0, k);
            head = frame.anchor;
          } else {
            prefixAnchor = frame.anchor || "";
            head = prefixAnchor;
          }
        } else {
          records = [...this.snap.records];
          head = this.snap.head;
          prefixAnchor = this.snap.prefixAnchor;
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
          tool: frame.tool && frame.tool.call_id ? { callID: frame.tool.call_id, name: frame.tool.name || "", args: frame.tool.args || "", awaitingApproval: !!frame.tool.awaiting_approval } : null,
          // The snapshot restores a PENDING card from the awaiting
          // tool slot (a reconnect must not lose the decision); a
          // resolved-outcome display is transient and does not
          // survive a reconnect (the tool result will land).
          approval: frame.tool && frame.tool.awaiting_approval && frame.tool.item
            ? { item: frame.tool.item, name: frame.tool.name || "", args: frame.tool.args || "", outcome: null }
            : null,
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
        // The durable result supersedes the transient executing slot.
        const tool = frame.r.t === "tool.result" ? null : this.snap.tool;
        // The committed record supersedes the live item by ItemID.
        const live = this.snap.live && this.snap.live.id !== frame.r.item ? this.snap.live : dropLiveIfEmpty(this.snap.live, frame.r);
        this.commit({ records, head, live, tool, verified: null });
        break;
      }
      case "event": {
        this.applyEvent(frame);
        break;
      }
      case "status": {
        const running = frame.running ?? this.snap.running;
        this.commit({
          running,
          // A settled run has no tool in flight (the server's Running
          // slot exists only inside a run).
          tool: running ? this.snap.tool : null,
          // ...and no approval either: the wait died with the run.
          approval: running ? this.snap.approval : null,
          // Absent compacting = no change (same rule as running):
          // notice-bearing frames omit positional fields on purpose,
          // and !!undefined would clobber a live compacting=true.
          compacting: frame.compacting ?? this.snap.compacting,
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
        // A record with this ItemID already landed (the wire can
        // reorder around a reconnect, and the server drain is a
        // best-effort): the durable record supersedes any transient
        // item — do not resurrect a live row for it.
        if (this.snap.records.some(r => r.item === frame.item)) break;
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
        // The executing slot lives on the SNAPSHOT, not the live item:
        // the assistant.message record lands (dropping the live item)
        // BEFORE its tool calls execute — the events arrive with no
        // live item to hang them on.
        if (!frame.call_id) break;
        this.commit({
          tool: { callID: frame.call_id, name: frame.name || "", args: frame.args || "", awaitingApproval: false },
        });
        const live = this.snap.live;
        if (live) {
          const parts = upsertTool(live.parts, frame.call_id, frame.name || "", frame.args || "", frame.t === "tool_call_done");
          this.commit({ live: { ...live, parts } });
        }
        break;
      }
      case "tool_result": {
        if (this.snap.tool && frame.call_id === this.snap.tool.callID) {
          this.commit({ tool: null });
          // The tool the card was about is done — the outcome line
          // has nowhere to live anymore.
          if (this.snap.approval) this.commit({ approval: null });
        }
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
      case "tool_approval_pending": {
        if (!frame.item) break;
        this.commit({
          approval: { item: frame.item, name: frame.name || "", args: frame.args || "", outcome: null },
        });
        break;
      }
      case "tool_approval_resolved": {
        // The outcome display rides the card until the tool completes
        // (tool_result clears it) or a new pending replaces it. An
        // unmatched resolution (a reconnect gap ate the pending) is
        // not an error — there is nothing to collapse.
        if (!frame.item || !this.snap.approval || this.snap.approval.item !== frame.item) break;
        this.commit({
          approval: { ...this.snap.approval, outcome: frame.outcome || "" },
        });
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
    const { getHistory, APIError } = await import("./api");
    let data;
    try {
      data = await getHistory(session, prefixAnchor);
    } catch (e) {
      // A compaction truncated the server's record view mid-flight:
      // our prefix anchor belongs to the truncated-away chain. The
      // reissued snapshot has (or will) reset the store's anchor —
      // retry once with the fresh one; a second stale answer means we
      // are at the head of the active segment.
      if (!(e instanceof APIError) || e.status !== 409) return false;
      const fresh = this.snap.prefixAnchor;
      if (!fresh || fresh === prefixAnchor) {
        this.commit({ more: false });
        return false;
      }
      try {
        data = await getHistory(session, fresh);
      } catch {
        this.commit({ more: false });
        return false;
      }
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
    // Key the part by the tool CALL id — the continuing events
    // (tool_result) address it that way, so the eventual result
    // attaches in place. The item id is the future tool.result
    // record's own identity, NOT the call's.
    parts.push({
      type: "tool-call",
      toolCallId: frame.tool.call_id || frame.tool.item,
      toolName: frame.tool.name,
      argsText: frame.tool.args || "",
      running: true,
    });
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
