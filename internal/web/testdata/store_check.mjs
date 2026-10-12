// Mirror-consistency check: the chat client's SessionStore (TypeScript,
// run directly via node type stripping) and the log page's reducer
// (reducer.mjs) implement the SAME state machine — snapshot-first
// resume, digest-chain fold, stale-anchor overlap rebuild. This script
// feeds identical frame sequences to both and asserts agreement on
// every shared field after each step, so a drift between the two
// implementations cannot land silently (both page-state bugs of the
// first review round lived exactly in this blind spot).
//
// Run by TestChatStoreMirror via `node store_check.mjs`. Serverless:
// the frames are synthetic; the server contract itself is pinned by
// reducer_check.mjs against a live serve.
import { initialState, initChain, applySnapshot, applyRecord } from "../static/reducer.mjs";
import { SessionStore } from "../../../clients/chat/src/store.ts";
import { chainGenesis, chainStep } from "../static/chain.mjs";

const failures = [];
function assert(cond, what) {
  if (!cond) failures.push(what);
}

// Minimal record factory: valid chainInput shape.
let n = 0;
const rec = (t, extra) => ({
  item: "it" + ++n,
  t,
  ts: "2026-10-10T00:00:0" + (n % 10) + "Z",
  ...extra,
});

async function fold(from, records) {
  let h = from;
  for (const r of records) h = await chainStep(h, r);
  return h;
}

const session = "scheck0";

// The two implementations under test.
const red = initialState();
await initChain(red, session);
const store = new SessionStore();
store.snap = { ...store.snap, session, head: red.head, prefixAnchor: red.prefixAnchor };

// apply() is TS-private; from plain JS it is the seam this check drives.
const apply = (frame) => store.apply(frame);

// agree() compares every SHARED field of the two state machines.
function agree(step) {
  assert(store.snap.records.length === red.records.length, step + ": records.length");
  assert(store.snap.head === red.head, step + ": head");
  assert(store.snap.prefixAnchor === red.prefixAnchor, step + ": prefixAnchor");
  assert(store.snap.more === red.more, step + ": more");
  assert(
    JSON.stringify(store.snap.records) === JSON.stringify(red.records),
    step + ": records content",
  );
}

// ---- 1. window snapshot: clean rebuild from the server anchor ----
const w = [rec("user.input", { content: "m1" }), rec("assistant.message", { content: "a1" }), rec("user.input", { content: "m2" })];
// Wire semantics: the snapshot anchor is the digest BEFORE the first
// delivered record (for a full window over a fresh state, genesis);
// d carries the digest AFTER them.
const windowDigest = await fold(red.head, w);
{
  const frame = { kind: "snapshot", anchor: red.head, resumed: false, more: true, records: w, running: true, d: windowDigest };
  await applySnapshot(red, frame);
  await apply(frame);
  agree("s1 window");
  assert(store.snap.running === true, "s1 running");
  assert(store.snap.verified === true, "s1 verified against the after-digest");
}

// ---- 2. a committed record appends on both ----
const r4 = rec("assistant.message", { content: "a2" });
{
  const frame = { kind: "record", r: r4 };
  await applyRecord(red, r4);
  await apply(frame);
  agree("s2 record");
}

// ---- 3. status frame ----
{
  const frame = { kind: "status", running: false, compacting: false, d: store.snap.head };
  await apply(frame);
  assert(store.snap.running === false, "s3 running");
  assert(store.snap.verified === true, "s3 verified on match");
}

// ---- 4. live events build the in-flight item (store-only surface) ----
{
  await apply({ kind: "event", t: "assistant_message_started", item: "live1" });
  await apply({ kind: "event", t: "reasoning_delta", item: "live1", text: "think " });
  await apply({ kind: "event", t: "reasoning_delta", item: "live1", text: "hard" });
  await apply({ kind: "event", t: "text_delta", item: "live1", text: "answer" });
  const live = store.snap.live;
  assert(live && live.id === "live1", "s4 live id");
  assert(live.parts.length === 2 && live.parts[0].type === "reasoning" && live.parts[0].text === "think hard", "s4 reasoning accumulated");
  assert(live.parts[1].type === "text" && live.parts[1].text === "answer", "s4 text accumulated");
  agree("s4 live (records untouched)");
}

// ---- 5. the committed record with the same ItemID supersedes live ----
{
  const r5 = rec("assistant.message", { item: "live1", content: "answer" });
  await apply({ kind: "record", r: r5 });
  await applyRecord(red, r5);
  assert(store.snap.live === null, "s5 live superseded");
  agree("s5 supersede");
}

// ---- 6. steer queue mirror (store-only surface) ----
{
  await apply({ kind: "event", t: "steer_queued", item: "st1", text: "v1" });
  assert(store.snap.steered.length === 1 && store.snap.steered[0].text === "v1", "s6 queued");
  await apply({ kind: "event", t: "steer_updated", item: "st1", text: "v2" });
  assert(store.snap.steered[0].text === "v2", "s6 updated");
  await apply({ kind: "event", t: "steer_withdrawn", item: "st1" });
  assert(store.snap.steered.length === 0, "s6 withdrawn");
}

// ---- 7. clean resume: suffix appends, anchors do not move ----
{
  const before = { head: store.snap.head, prefix: store.snap.prefixAnchor, n: store.snap.records.length };
  const r6 = rec("user.input", { content: "m3" });
  const frame = {
    kind: "snapshot",
    anchor: before.head,
    resumed: true,
    more: true,
    records: [r6],
    running: false,
    d: await fold(before.head, [r6]),
  };
  await applySnapshot(red, frame);
  await apply(frame);
  agree("s7 clean resume");
  assert(store.snap.records.length === before.n + 1, "s7 suffix only");
  assert(store.snap.prefixAnchor === before.prefix, "s7 cursor preserved");
}

// ---- 8. the stale-anchor overlap reconnect ----
// The client holds [w, r4, r5, r6]; the reconnect URL froze at the
// digest after r5. The replayed suffix (r6, r7) overlaps r6.
{
  const staleAnchor = store.snap.records.length ? await fold(store.snap.prefixAnchor, store.snap.records.slice(0, -1)) : store.snap.prefixAnchor;
  const suffix = store.snap.records.slice(-1); // r6 replayed
  const r7 = rec("user.input", { content: "m4" });
  const full = [...suffix, r7];
  const frame = {
    kind: "snapshot",
    anchor: staleAnchor,
    resumed: true,
    more: true,
    records: full,
    running: false,
    d: await fold(staleAnchor, full),
  };
  await applySnapshot(red, frame);
  await apply(frame);
  agree("s8 overlap rebuild");
  assert(store.snap.records[store.snap.records.length - 1].item === r7.item, "s8 suffix appended");
  assert(store.snap.head === frame.d, "s8 head = server digest");
  // The overlap must not duplicate r6:
  const ids = store.snap.records.map(r => r.item);
  assert(new Set(ids).size === ids.length, "s8 no duplicate records");
}

// ---- 9. unknown anchor degrades to the window rebuild ----
{
  const r8 = rec("assistant.message", { content: "a3" });
  const frame = {
    kind: "snapshot",
    anchor: "deadbeef",
    resumed: true,
    more: false,
    records: [r8],
    running: false,
    d: await fold("deadbeef", [r8]),
  };
  await applySnapshot(red, frame);
  await apply(frame);
  agree("s9 unknown anchor");
  assert(store.snap.records.length === 1 && store.snap.records[0].item === r8.item, "s9 rebuilt from anchor");
  assert(store.snap.prefixAnchor === "deadbeef", "s9 cursor moved to anchor");
}

// ---- 10. the burst: frames fired SYNCHRONOUSLY through onmessage ----
// apply awaits the digest fold (async crypto); without serialization,
// same-tick frames read the same stale snapshot and the later commit
// silently drops the earlier record (and a status frame can report
// drift on a chain still being folded). The store must serialize.
{
  const created = [];
  class FakeES {
    constructor() {
      this.onmessage = null;
      this.onerror = null;
    }
    close() {}
  }
  const burstStore = new SessionStore(url => {
    const es = new FakeES();
    created.push(es);
    return es;
  });
  await burstStore.open("burst-session");
  assert(created.length === 1, "s10 es created");

  const w = [rec("user.input", { content: "b1" }), rec("assistant.message", { content: "b2" }), rec("user.input", { content: "b3" })];
  const genesisB = await chainGenesis("burst-session");
  let h = genesisB;
  for (const r of w) h = await chainStep(h, r);
  const anchorB = genesisB; // wire: anchor = digest BEFORE the window
  const r4 = rec("assistant.message", { content: "b4" });
  const r5 = rec("user.input", { content: "b5" });
  const h4 = await chainStep(h, r4);
  const h5 = await chainStep(h4, r5);

  // ONE synchronous burst: snapshot + two records + a status frame
  // carrying the FINAL digest. No awaits between deliveries.
  const send = f => created[0].onmessage({ data: JSON.stringify(f) });
  send({ kind: "snapshot", anchor: anchorB, resumed: false, more: false, records: w, running: false, d: h });
  send({ kind: "record", r: r4 });
  send({ kind: "record", r: r5 });
  send({ kind: "status", running: false, d: h5 });

  // Drain the queue (deterministically: wait for the record count).
  for (let i = 0; i < 200 && burstStore.snap.records.length < 5; i++) {
    await new Promise(r => setTimeout(r, 5));
  }
  const bs = burstStore.snap;
  assert(bs.records.length === 5, "s10 no records lost in the burst: " + bs.records.length);
  assert(bs.head === h5, "s10 head converged");
  assert(bs.verified === true, "s10 status frame did not report drift (serialized behind the records)");
  const idsB = bs.records.map(r => r.item);
  assert(new Set(idsB).size === 5, "s10 no duplicates");
}

// ---- 11. a started event for an item whose record already landed ----
// The wire reorder guard: no live row may be resurrected for an item
// the durable record already closed.
{
  const r = rec("assistant.message", { item: "superseded-1", content: "done already" });
  await apply({ kind: "record", r });
  await applyRecord(red, r);
  const before = store.snap.live;
  await apply({ kind: "event", t: "assistant_message_started", item: "superseded-1" });
  assert(store.snap.live === before, "s11 started after record does not resurrect a live item");
  await apply({ kind: "event", t: "text_delta", item: "superseded-1", text: "late" });
  assert(store.snap.live === before, "s11 late deltas attach to nothing");
}

// ---- 12. the in-flight tool slot ----
// The assistant record lands BEFORE its tools execute, so the events
// arrive with no live item: the executing state lives on the snapshot
// slot and overlays the record's call part.
{
  const am = rec("assistant.message", { content: "calling", tool_calls: [{ id: "c9", name: "shell", arguments: "{}" }] });
  await apply({ kind: "record", r: am });
  await applyRecord(red, am);
  assert(store.snap.tool === null, "s12 no slot before tool_started");
  await apply({ kind: "event", t: "tool_started", item: am.item, call_id: "c9", name: "shell", args: "{}" });
  assert(store.snap.tool && store.snap.tool.callID === "c9", "s12 slot set by tool_started");
  await apply({ kind: "event", t: "tool_result", call_id: "c9", name: "shell", text: "out" });
  assert(store.snap.tool === null, "s12 slot cleared by tool_result event");
  // Again, cleared by the durable record instead.
  await apply({ kind: "event", t: "tool_started", item: am.item, call_id: "c9", name: "shell", args: "{}" });
  const tr = rec("tool.result", { tool_call_id: "c9", tool_name: "shell", content: "out" });
  await apply({ kind: "record", r: tr });
  await applyRecord(red, tr);
  assert(store.snap.tool === null, "s12 slot cleared by the tool.result record");
  // A settled run clears it too.
  await apply({ kind: "event", t: "tool_started", item: am.item, call_id: "c9", name: "shell", args: "{}" });
  await apply({ kind: "status", running: false, d: store.snap.head });
  assert(store.snap.tool === null, "s12 slot cleared by the settled status");
  // The reconnect snapshot: tool without streaming still sets the slot.
  await apply({
    kind: "snapshot",
    anchor: store.snap.prefixAnchor,
    resumed: true,
    records: [],
    running: true,
    streaming: undefined,
    tool: { item: "i-tool", call_id: "c9", name: "shell", args: "{}" },
    d: store.snap.head,
  });
  assert(store.snap.tool && store.snap.tool.callID === "c9", "s12 snapshot sets the slot (tool without streaming)");
}

// ---- 13. events trailing their records are idempotent ----
// The run commits completed/tool_result records BEFORE emitting their
// events, so the wire may deliver the event after the record. The
// client must no-op: no live resurrection, no state damage.
{
  const r = rec("assistant.message", { item: "trail-1", content: "done" });
  await apply({ kind: "record", r });
  await applyRecord(red, r);
  const before = JSON.stringify({ live: store.snap.live, tool: store.snap.tool, n: store.snap.records.length });
  await apply({ kind: "event", t: "assistant_message_completed", item: "trail-1" });
  const trr = rec("tool.result", { tool_call_id: "c-trail", tool_name: "shell", content: "out" });
  await apply({ kind: "event", t: "tool_result", call_id: "c-trail", name: "shell", text: "out" });
  const after = JSON.stringify({ live: store.snap.live, tool: store.snap.tool, n: store.snap.records.length });
  assert(before === after, "s13 trailing events changed state");
}

if (failures.length) {
  console.error("store_check: FAILED");
  for (const f of failures) console.error("  - " + f);
  process.exit(1);
}
console.log("store_check: OK (13 scenarios: mirror + burst + reorder guard + tool slot + trailing idempotence)");
