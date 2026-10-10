// The page's state machine, DOM-free: the chain, the anchors, and the
// record store. app.js maps the returned effects onto the DOM; the node
// fixture test (reducer_check.mjs) drives this exact module against a
// live server — the two page-state bugs of the first review round
// (prefixAnchor overwritten on resume, the chain folded from genesis
// instead of the anchor) both lived here, in CI's blind spot.
import { chainGenesis, chainStep } from "./chain.mjs";

export function initialState() {
  return {
    session: "",
    head: "",         // digest after all folded records
    prefixAnchor: "", // digest BEFORE the first held record (history cursor)
    records: [],      // the page's record store
    more: false,      // history exists before the prefix anchor
    running: false,
  };
}

// initChain establishes the pre-snapshot placeholders: the genesis of
// the session's chain. The reducer is the sole owner of the chain
// fields — app.js never writes head/prefixAnchor directly.
export async function initChain(state, sessionID) {
  state.session = sessionID;
  state.head = await chainGenesis(sessionID);
  state.prefixAnchor = state.head;
}

// A snapshot either resumes (the suffix appends to what we hold; the
// anchors do not move) or reissues a window (a clean rebuild whose fold
// starts from the server-attested anchor — never genesis: a running
// hash converges to the same head from any correct anchor, and genesis
// is only correct when nothing was truncated).
export async function applySnapshot(state, frame) {
  const effects = { reset: false, appended: [] };
  if (!frame.resumed) {
    state.records = [];
    state.head = frame.anchor;
    state.prefixAnchor = frame.anchor;
    state.more = !!frame.more;
    effects.reset = true;
  }
  for (const r of frame.records || []) {
    state.records.push(r);
    state.head = await chainStep(state.head, r);
    effects.appended.push(r);
  }
  state.running = !!frame.running;
  return effects;
}

export async function applyRecord(state, record) {
  state.records.push(record);
  state.head = await chainStep(state.head, record);
}

export function applyStatus(state, frame) {
  if (frame.running !== undefined) state.running = !!frame.running;
}

// A scroll-back batch. Backward verification is stronger than forward:
// both ends are known — fold(data.anchor, records) must equal our
// prefix anchor. On success the records prepend and the cursor moves;
// the head never changes (history extension cannot alter a running
// hash's tail). Returns "ok" or "drift".
export async function applyHistory(state, data) {
  let h = data.anchor;
  for (const r of data.records) {
    h = await chainStep(h, r);
  }
  if (h !== state.prefixAnchor) {
    return "drift";
  }
  for (const r of data.records) {
    state.records.unshift(r);
  }
  state.prefixAnchor = data.anchor;
  state.more = !!data.more;
  return "ok";
}
