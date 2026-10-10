// E2E page-state fixture: drives the page's actual reducer (reducer.mjs)
// against a live server through the three lifecycle scenarios — first
// window, resume-after-disconnect, scroll-back after resume — asserting
// the {head, prefixAnchor, more, records.length} evolution. Run by
// TestPageReducerFixture via `node reducer_check.mjs <base-url>`.
import { initialState, initChain, applySnapshot, applyRecord, applyStatus, applyHistory } from "./reducer.mjs";

const base = process.argv[2];
if (!base) {
  console.error("reducer_check: missing base url");
  process.exit(2);
}
const session = process.argv[3];
if (!session) {
  console.error("reducer_check: missing session id");
  process.exit(2);
}
// Session-scoped routes (docs/multi-session.md §4).
const api = (suffix) => "/api/s/" + session + suffix;

function assert(cond, what) {
  if (!cond) {
    console.error("reducer_check: FAILED at " + what);
    process.exit(1);
  }
}

async function firstFrame(url) {
  const res = await fetch(url);
  if (!res.ok) {
    console.error("reducer_check: " + url + " -> " + res.status);
    process.exit(1);
  }
  const reader = res.body.getReader();
  const dec = new TextDecoder();
  let buf = "";
  for (;;) {
    const { done, value } = await reader.read();
    if (done) throw new Error("stream ended before a frame");
    buf += dec.decode(value, { stream: true });
    const idx = buf.indexOf("\n\n");
    if (idx > 0) {
      const line = buf.slice(0, idx);
      await reader.cancel();
      if (!line.startsWith("data: ")) throw new Error("not a data line");
      return JSON.parse(line.slice(6));
    }
  }
}

async function post(path, body) {
  const res = await fetch(base + path, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  return res;
}

async function waitSettled(countAtLeast) {
  for (let i = 0; i < 200; i++) {
    const s = await (await fetch(base + api("/state"))).json();
    if (!s.running && s.count >= countAtLeast) return s;
    await new Promise(r => setTimeout(r, 25));
  }
  throw new Error("run never settled");
}

const state = initialState();
const st0 = await (await fetch(base + api("/state"))).json();
state.session = st0.session;

// Scenario 1: first connect, truncated window.
const snap1 = await firstFrame(base + api("/stream?window=50"));
const fx1 = await applySnapshot(state, snap1);
assert(!snap1.resumed, "s1 not resumed");
assert(fx1.reset, "s1 reset");
assert(state.records.length === 50, "s1 holds 50");
assert(state.more === true, "s1 more=true");
assert(state.prefixAnchor === snap1.anchor, "s1 prefixAnchor = anchor");
assert(state.head === snap1.d && state.head === st0.digest, "s1 head = server head");

// Scenario 2: a new run lands, then a reconnect with from=head resumes.
const before = { head: state.head, prefix: state.prefixAnchor, n: state.records.length, more: state.more };
const sent = await post(api("/send"), { text: "resume probe" });
assert(sent.ok, "s2 send accepted");
const st1 = await waitSettled(st0.count + 2);
const snap2 = await firstFrame(base + api("/stream?from=") + state.head);
const fx2 = await applySnapshot(state, snap2);
assert(snap2.resumed, "s2 resumed");
assert(!fx2.reset, "s2 no reset");
assert(snap2.records.length === 2, "s2 suffix = 2 records");
assert(state.records.length === before.n + 2, "s2 appended");
assert(state.prefixAnchor === before.prefix, "s2 prefixAnchor unchanged");
assert(snap2.anchor === before.head, "s2 server anchor = our head");
assert(state.more === before.more, "s2 more untouched");
assert(state.head === snap2.d && state.head === st1.digest, "s2 head = server head");

// Scenario 3: scroll-back after resume — prefix moves, head does not.
const headAfterResume = state.head;
const data = await (await fetch(base + api("/history?before=") + encodeURIComponent(state.prefixAnchor) + "&limit=20")).json();
const out = await applyHistory(state, data);
assert(out === "ok", "s3 backfill ok");
assert(state.records.length === before.n + 2 + data.records.length, "s3 prepended");
assert(state.prefixAnchor === data.anchor, "s3 prefix = data.anchor");
assert(state.head === headAfterResume, "s3 head unchanged");
assert(state.more === !!data.more, "s3 more tracks server");

console.log("reducer_check: OK (window 50, resume +" + snap2.records.length +
  ", backfill +" + data.records.length + ")");

// Scenario 4: the multi-session switch semantics (data level — the DOM
// generation guard lives in app.js; here we pin what the page's
// per-session cache and reset must achieve). argv[3] is a second
// session id in the same project.
const secondID = process.argv[4];
if (secondID) {
  // (a) a switch is a FULL reset: a fresh state's genesis differs from
  // the held session's head chain.
  const stateB = initialState();
  await initChain(stateB, secondID);
  assert(stateB.head !== state.head, "s4 fresh session has its own genesis");
  assert(stateB.records.length === 0, "s4 fresh state is empty");

  // (b) the switch-away cache: a shallow copy of the held records plus
  // the cursor fields. Switching back rehydrates and resumes from the
  // held head — the server replays only the suffix.
  const cache = {
    records: state.records.slice(),
    head: state.head,
    prefixAnchor: state.prefixAnchor,
    more: state.more,
  };
  const restored = initialState();
  restored.session = state.session;
  restored.records = cache.records.slice();
  restored.head = cache.head;
  restored.prefixAnchor = cache.prefixAnchor;
  restored.more = cache.more;
  // A record landed on session A while we were "away" (B's view).
  const sentA = await post(api("/send"), { text: "while away" });
  assert(sentA.ok, "s4 send on A accepted");
  await waitSettled(cache.records.length + 2);
  const snapA = await firstFrame(base + api("/stream?from=") + cache.head);
  const fxA = await applySnapshot(restored, snapA);
  assert(snapA.resumed, "s4 switch-back resumed from cached head");
  assert(!fxA.reset, "s4 no reset on switch-back");
  assert(restored.records.length === cache.records.length + 2, "s4 suffix only");
  assert(restored.prefixAnchor === cache.prefixAnchor, "s4 cursor preserved");
  assert(restored.head === snapA.d, "s4 head = server head after switch-back");

  // (c) the cached original is untouched by the restore's mutations.
  assert(cache.records.length !== restored.records.length, "s4 cache not aliased");
  console.log("reducer_check: OK (switch: reset + cache resume +" + snapA.records.length + ")");
}

// Scenario 5: the STALE-ANCHOR reconnect. The browser's EventSource
// auto-reconnects with its original URL — the from digest froze at
// connect time — so if records landed and were folded meanwhile, the
// replayed suffix OVERLAPS what we hold. Appending would duplicate;
// the rebuild must keep the pre-anchor prefix (history pages included)
// and end at the server head with no duplicates.
{
  const staleHead = state.head;
  const n = state.records.length;
  const sent5 = await post(api("/send"), { text: "stale anchor probe" });
  assert(sent5.ok, "s5 send accepted");
  await waitSettled(n + 2);
  // The reconnect frame the server produces for the stale URL.
  const snap5 = await firstFrame(base + api("/stream?from=") + staleHead);
  assert(snap5.resumed, "s5 stale reconnect resumed");
  assert(snap5.anchor === staleHead, "s5 anchor echoes the stale digest");
  const fx5 = await applySnapshot(state, snap5);
  assert(!fx5.reset, "s5 overlap rebuild is not a reset");
  assert(state.records.length === n + snap5.records.length, "s5 no duplicates");
  assert(state.head === snap5.d, "s5 head = server head after rebuild");
  console.log("reducer_check: OK (stale anchor rebuild +" + snap5.records.length + ")");
}
