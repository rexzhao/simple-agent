// E2E page-state fixture: drives the page's actual reducer (reducer.mjs)
// against a live server through the three lifecycle scenarios — first
// window, resume-after-disconnect, scroll-back after resume — asserting
// the {head, prefixAnchor, more, records.length} evolution. Run by
// TestPageReducerFixture via `node reducer_check.mjs <base-url>`.
import { initialState, applySnapshot, applyRecord, applyStatus, applyHistory } from "./reducer.mjs";

const base = process.argv[2];
if (!base) {
  console.error("reducer_check: missing base url");
  process.exit(2);
}

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
    const s = await (await fetch(base + "/api/state")).json();
    if (!s.running && s.count >= countAtLeast) return s;
    await new Promise(r => setTimeout(r, 25));
  }
  throw new Error("run never settled");
}

const state = initialState();
const st0 = await (await fetch(base + "/api/state")).json();
state.session = st0.session;

// Scenario 1: first connect, truncated window.
const snap1 = await firstFrame(base + "/api/stream?window=50");
const fx1 = await applySnapshot(state, snap1);
assert(!snap1.resumed, "s1 not resumed");
assert(fx1.reset, "s1 reset");
assert(state.records.length === 50, "s1 holds 50");
assert(state.more === true, "s1 more=true");
assert(state.prefixAnchor === snap1.anchor, "s1 prefixAnchor = anchor");
assert(state.head === snap1.d && state.head === st0.digest, "s1 head = server head");

// Scenario 2: a new run lands, then a reconnect with from=head resumes.
const before = { head: state.head, prefix: state.prefixAnchor, n: state.records.length, more: state.more };
const sent = await post("/api/send", { text: "resume probe" });
assert(sent.ok, "s2 send accepted");
const st1 = await waitSettled(st0.count + 2);
const snap2 = await firstFrame(base + "/api/stream?from=" + state.head);
const fx2 = await applySnapshot(state, snap2);
assert(snap2.resumed, "s2 resumed");
assert(!fx2.reset, "s2 no reset");
assert(snap2.records.length === 2, "s2 suffix = 2 records");
assert(state.records.length === before.n + 2, "s2 appended");
assert(state.prefixAnchor === before.prefix, "s2 prefixAnchor unchanged");
assert(state.head === snap2.d && state.head === st1.digest, "s2 head = server head");

// Scenario 3: scroll-back after resume — prefix moves, head does not.
const headAfterResume = state.head;
const data = await (await fetch(base + "/api/history?before=" + encodeURIComponent(state.prefixAnchor) + "&limit=20")).json();
const out = await applyHistory(state, data);
assert(out === "ok", "s3 backfill ok");
assert(state.records.length === before.n + 2 + data.records.length, "s3 prepended");
assert(state.prefixAnchor === data.anchor, "s3 prefix = data.anchor");
assert(state.head === headAfterResume, "s3 head unchanged");
assert(state.more === !!data.more, "s3 more tracks server");

console.log("reducer_check: OK (window 50, resume +" + snap2.records.length +
  ", backfill +" + data.records.length + ")");
