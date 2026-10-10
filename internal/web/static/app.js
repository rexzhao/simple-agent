// sai web log view. The page's contract (docs/webui.md):
//   page store == anchor prefix (server-attested) ∪ received record stream
// verified continuously by recomputing the digest chain over what the page
// holds and comparing it against the server's head in every status frame.
"use strict";

import { chainGenesis, chainStep } from "./chain.mjs";
import { initialState, applySnapshot, applyRecord, applyStatus, applyHistory } from "./reducer.mjs";

// ---------- state ----------

// The chain/anchor/record store lives in the reducer (shared with the
// fixture test); the page adds the DOM map and the connection bits.
const state = Object.assign(initialState(), {
  byItem: new Map(),  // itemID -> row element (committed + in-flight)
  es: null,
  reconnectFrom: "",
  streaming: null,    // {item} of the in-flight assistant message
});

const rowsEl = document.getElementById("rows");
const logEl = document.getElementById("log");

// Frame processing is serialized through one promise chain: the chain
// folds are async (crypto.subtle), and two frames processed concurrently
// would both fold from the same predecessor head. Scroll-back shares the
// same chain so history prepends never interleave with live folds.
let queue = Promise.resolve();
function enqueue(job) {
  queue = queue.then(job).catch(err => console.error("sai page:", err));
  return queue;
}

// ---------- rendering (escaped, log-style, no chat bubbles) ----------

function textNode(text) { return document.createTextNode(text); }

function recordBody(r) {
  const body = document.createDocumentFragment();
  if (r.reasoning) {
    const div = document.createElement("div");
    div.className = "reasoning";
    div.appendChild(textNode(r.reasoning));
    body.appendChild(div);
  }
  if (r.content) {
    const div = document.createElement("div");
    div.appendChild(textNode(r.content));
    body.appendChild(div);
  }
  for (const call of r.tool_calls || []) {
    const div = document.createElement("div");
    div.appendChild(textNode(call.name + " " + (call.arguments || "").slice(0, 500)));
    body.appendChild(div);
  }
  if (r.error) {
    const div = document.createElement("div");
    div.className = "error-text";
    div.appendChild(textNode(r.error));
    body.appendChild(div);
  }
  if (!body.childNodes.length) {
    const div = document.createElement("div");
    div.style.color = "var(--dim)";
    div.appendChild(textNode("·"));
    body.appendChild(div);
  }
  return body;
}

function rowFor(r, streaming) {
  const row = document.createElement("div");
  row.className = "row " + (r.t === "user.input" ? "user" : "") +
    (r.t === "run.failed" ? "failed" : "") + (streaming ? "streaming" : "");
  const ts = document.createElement("span");
  ts.className = "ts";
  ts.appendChild(textNode((r.ts || "").slice(11, 19)));
  const type = document.createElement("span");
  type.className = "type";
  type.appendChild(textNode(r.item ? r.t + " " + r.item.slice(0, 8) : r.t));
  const body = document.createElement("span");
  body.className = "body";
  row.append(ts, type, body);
  row._body = body;
  return row;
}

function appendRecord(r) {
  if (state.byItem.has(r.item)) {
    // The ItemID merge: a committed record replaces the streamed view of
    // the same item.
    const existing = state.byItem.get(r.item);
    existing._body.replaceChildren(recordBody(r));
    existing.classList.remove("streaming");
    return;
  }
  const row = rowFor(r, false);
  row._body.appendChild(recordBody(r));
  rowsEl.appendChild(row);
  state.byItem.set(r.item, row);
}

// ---------- chain + self-check ----------

function setSync(ok, serverHead) {
  const el = document.getElementById("sync");
  el.classList.remove("sync-unknown", "synced", "drift");
  if (ok) {
    el.classList.add("synced");
    el.title = "digest chain verified";
    el.textContent = "✓ synced " + state.head.slice(0, 8);
  } else {
    el.classList.add("drift");
    el.title = "page " + state.head.slice(0, 12) + " ≠ server " + (serverHead || "").slice(0, 12);
    el.textContent = "✗ drift";
  }
}

// ---------- SSE ----------

function connect() {
  const url = state.reconnectFrom ? "/api/stream?from=" + encodeURIComponent(state.reconnectFrom) : "/api/stream";
  const es = new EventSource(url);
  state.es = es;
  es.onmessage = e => {
    let frame;
    try {
      frame = JSON.parse(e.data);
    } catch (err) {
      console.error("sai page: bad frame", err);
      return;
    }
    enqueue(() => dispatch(frame));
  };
  es.onerror = () => {
    // EventSource retries on its own; the in-memory digest lets the
    // retry resume with ?from= instead of replaying.
    state.reconnectFrom = state.head;
  };
}

async function dispatch(frame) {
  switch (frame.kind) {
    case "snapshot": {
      const fx = await applySnapshot(state, frame);
      if (fx.reset) {
        state.byItem = new Map();
        rowsEl.replaceChildren();
      }
      for (const r of fx.appended) appendRecord(r);
      scrollToBottom();
      state.reconnectFrom = "";
      document.getElementById("load-more").hidden = !state.more;
      state.running = !!frame.running;
      if (frame.streaming) beginStreamingItem(frame.streaming.item, frame.streaming);
      if (frame.tool) beginToolItem(frame.tool.item, frame.tool.name, frame.tool.args);
      updateStatus();
      if (frame.d) setSync(state.head === frame.d, frame.d);
      break;
    }
    case "record":
      await applyRecord(state, frame.r);
      appendRecord(frame.r);
      scrollToBottom();
      break;
    case "event":
      handleEvent(frame);
      break;
    case "status":
      applyStatus(state, frame);
      updateStatus();
      if (frame.notice) showNotice(frame.notice);
      if (frame.d) setSync(state.head === frame.d, frame.d);
      break;
  }
}

function updateStatus() {
  document.getElementById("session").textContent = "session " + state.session;
  document.getElementById("count").textContent = state.records.length + " records" + (state.more ? "+" : "");
  const run = document.getElementById("runstate");
  run.textContent = state.running ? "running" : "idle";
  run.className = state.running ? "running" : "idle";
  document.getElementById("send").disabled = state.running;
}

function showNotice(text) {
  document.getElementById("notices").textContent = text;
}

function showError(text) {
  const el = document.getElementById("error");
  el.textContent = text;
  el.classList.remove("hidden");
  setTimeout(() => el.classList.add("hidden"), 4000);
}

// ---------- in-flight items (deltas merged by identity) ----------

function beginStreamingItem(itemID, snapshot) {
  let row = state.byItem.get(itemID);
  if (!row) {
    const r = { t: "assistant.message", item: itemID, ts: "" };
    row = rowFor(r, true);
    rowsEl.appendChild(row);
    state.byItem.set(itemID, row);
    scrollToBottom();
  }
  if (snapshot) {
    row._body.replaceChildren(recordBody({ reasoning: snapshot.reasoning, content: snapshot.content }));
  }
  return row;
}

function beginToolItem(itemID, name, args) {
  let row = state.byItem.get(itemID);
  if (!row) {
    const r = { t: "tool.result", item: itemID, ts: "" };
    row = rowFor(r, true);
    state.byItem.set(itemID, row);
    rowsEl.appendChild(row);
    scrollToBottom();
  }
  if (name) {
    row._body.replaceChildren(recordBody({ tool_calls: [{ name, arguments: args }] }));
  }
  return row;
}

function handleEvent(frame) {
  switch (frame.t) {
    case "assistant_message_started":
      state.streaming = { item: frame.item };
      beginStreamingItem(frame.item);
      break;
    case "reasoning_delta":
    case "text_delta": {
      if (!state.streaming) break;
      const row = state.byItem.get(state.streaming.item);
      if (!row) break;
      const line = document.createElement("div");
      if (frame.t === "reasoning_delta") line.className = "reasoning";
      line.appendChild(textNode(frame.text));
      row._body.appendChild(line);
      scrollToBottom();
      break;
    }
    case "tool_started":
      beginToolItem(frame.item, frame.name, frame.args);
      break;
    case "assistant_message_completed":
    case "assistant_message_failed":
      if (state.streaming && state.streaming.item === frame.item) state.streaming = null;
      break;
    case "context_pressure":
      showNotice("context " + frame.est + " ≥ " + frame.limit + " tokens");
      break;
    case "provider_retry":
      showNotice("retrying (attempt " + frame.try + "/" + frame.try_max + ")");
      break;
    case "compaction_started":
      showNotice("compacting…");
      break;
  }
}

function scrollToBottom() {
  const nearBottom = logEl.scrollHeight - logEl.scrollTop - logEl.clientHeight < 200;
  if (nearBottom) logEl.scrollTop = logEl.scrollHeight;
}

// ---------- scroll-back ----------

document.getElementById("load-more").addEventListener("click", () => enqueue(loadHistory));
logEl.addEventListener("scroll", () => {
  if (logEl.scrollTop < 60 && state.more) enqueue(loadHistory);
});

async function loadHistory() {
  if (!state.more || !state.prefixAnchor) return;
  const res = await fetch("/api/history?before=" + encodeURIComponent(state.prefixAnchor) + "&limit=50");
  if (res.status === 409) {
    // Stale anchor (server restarted): resynchronize from scratch.
    state.reconnectFrom = "";
    state.es.close();
    connect();
    return;
  }
  const data = await res.json();
  // Backward verification and the store/cursor update live in the
  // reducer (shared with the fixture test); the head never changes —
  // history extension cannot alter a running hash's tail.
  const outcome = await applyHistory(state, data);
  if (outcome !== "ok") {
    setSync(false, "");
    state.more = false;
    document.getElementById("load-more").hidden = true;
    return;
  }
  // Prepend in order, keeping scroll anchored.
  const prevHeight = logEl.scrollHeight;
  for (const r of data.records) {
    const row = rowFor(r, false);
    row._body.appendChild(recordBody(r));
    if (rowsEl.firstChild) rowsEl.insertBefore(row, rowsEl.firstChild);
    else rowsEl.appendChild(row);
    state.byItem.set(r.item, row);
  }
  document.getElementById("load-more").hidden = !state.more;
  logEl.scrollTop += logEl.scrollHeight - prevHeight;
}

// ---------- composer ----------

document.getElementById("composer").addEventListener("submit", async e => {
  e.preventDefault();
  const input = document.getElementById("input");
  const text = input.value.trim();
  if (!text) return;
  const res = await fetch("/api/send", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ text }),
  });
  if (res.ok) {
    input.value = "";
  } else if (res.status === 409) {
    showError("a run is already active");
  } else {
    showError(await res.text());
  }
});

document.getElementById("stop").addEventListener("click", () => {
  fetch("/api/stop", { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" });
});

document.getElementById("compact").addEventListener("click", async () => {
  const res = await fetch("/api/compact", { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" });
  if (res.status === 409) {
    const data = await res.json().catch(() => ({}));
    showError(data.error === "nothing_to_compact" ? "nothing to compact" : "busy");
  }
});

// ---------- boot ----------

// Refresh the status area from /api/state while the stream is down
// (covers the gap between connects).
setInterval(async () => {
  if (state.es && state.es.readyState === 2) {
    try {
      const res = await fetch("/api/state");
      if (res.ok) {
        const s = await res.json();
        state.running = s.running;
        updateStatus();
      }
    } catch (_) { /* server away */ }
  }
}, 2000);

(async () => {
  // The genesis needs the served session id.
  try {
    const res = await fetch("/api/state");
    if (res.ok) {
      const s = await res.json();
      state.session = s.session;
      history.replaceState(null, "", "?session=" + s.session);
    }
  } catch (_) { /* fall back to the URL guess below */ }
  if (!state.session) {
    state.session = new URLSearchParams(location.search).get("session") || "local";
  }
  state.head = await chainGenesis(state.session);
  state.prefixAnchor = state.head;
  connect();
})();
