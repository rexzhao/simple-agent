// sai web log view. The page's contract (docs/webui.md):
//   page store == anchor prefix (server-attested) ∪ received record stream
// verified continuously by recomputing the digest chain over what the page
// holds and comparing it against the server's head in every status frame.
"use strict";

// ---------- digest chain (must match internal/web/wire.go byte for byte) ----------

const chainVersion = "sai/v1/";

async function sha256Hex(text) {
  const data = new TextEncoder().encode(text);
  const digest = await crypto.subtle.digest("SHA-256", data);
  return [...new Uint8Array(digest)].map(b => b.toString(16).padStart(2, "0")).join("");
}

function chainInput(r) {
  let s = "";
  const field = v => { s += v.length + ":" + v; };
  field(r.item || "");
  field(r.t || "");
  field(r.ts || "");
  field(r.content || "");
  field(r.reasoning || "");
  field(r.is_error ? "1" : "0");
  field(r.tool_call_id || "");
  field(r.tool_name || "");
  field(r.error || "");
  s += (r.tool_calls || []).length + "#";
  for (const call of r.tool_calls || []) {
    field(call.name || "");
    field(call.arguments || "");
    field(call.id || "");
  }
  field(r.provider || "");
  field(r.model || "");
  field(r.cwd || "");
  return s;
}

// ---------- state ----------

const state = {
  session: "",
  head: chainVersion, // digest after all folded records
  records: [],        // the page's record store
  byItem: new Map(),  // itemID -> row element (committed + in-flight)
  anchorStart: 0,     // index of the first held record
  more: false,
  running: false,
  streaming: null,    // {item, reasoning, text} for the in-flight row
  es: null,
  reconnectFrom: "",
};

const rowsEl = document.getElementById("rows");
const logEl = document.getElementById("log");

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
  if (!r.reasoning && !r.content && (!r.tool_calls || !r.tool_calls.length) && !r.error) {
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

async function foldRecord(r) {
  state.head = await sha256Hex(state.head + "\n" + chainInput(r));
}

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

async function handleRecords(records) {
  for (const r of records) {
    state.records.push(r);
    await foldRecord(r);
    appendRecord(r);
  }
  scrollToBottom();
}

// ---------- SSE ----------

function connect() {
  const url = state.reconnectFrom ? "/api/stream?from=" + encodeURIComponent(state.reconnectFrom) : "/api/stream";
  const es = new EventSource(url);
  state.es = es;
  es.onmessage = e => dispatch(JSON.parse(e.data));
  es.onerror = () => {
    // EventSource retries on its own; the in-memory digest lets the
    // retry resume with ?from= instead of replaying.
    state.reconnectFrom = state.head;
  };
}

async function dispatch(frame) {
  switch (frame.kind) {
    case "snapshot":
      if (!frame.resumed) {
        // Rule A: a miss is a clean rebuild, never a chain continuation.
        state.records = [];
        state.byItem = new Map();
        rowsEl.replaceChildren();
        state.head = await sha256Hex(chainVersion + sessionIdFromUrl());
      }
      state.reconnectFrom = "";
      await handleRecords(frame.records || []);
      state.more = !!frame.more;
      document.getElementById("load-more").hidden = !state.more;
      state.running = !!frame.running;
      if (frame.streaming) beginStreamingItem(frame.streaming.item_id || frame.streaming.ItemID, frame.streaming);
      updateStatus();
      // First live check after the snapshot.
      if (frame.d) setSync(state.head === frame.d, frame.d);
      break;
    case "record":
      await handleRecords([frame.r]);
      break;
    case "event":
      handleEvent(frame);
      break;
    case "status":
      state.running = !!frame.running;
      updateStatus();
      if (frame.notice) showNotice(frame.notice);
      if (frame.d) setSync(state.head === frame.d, frame.d);
      break;
  }
}

function sessionIdFromUrl() {
  return new URLSearchParams(location.search).get("session") || "local";
}

function updateStatus() {
  document.getElementById("session").textContent = "session " + sessionIdFromUrl();
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

// ---------- in-flight item (deltas merged by identity) ----------

function beginStreamingItem(itemID, snapshotItem) {
  let row = state.byItem.get(itemID);
  if (!row) {
    const r = { t: "assistant.message", item: itemID, ts: "" };
    row = rowFor(r, true);
    rowsEl.appendChild(row);
    state.byItem.set(itemID, row);
    scrollToBottom();
  }
  if (snapshotItem) {
    const msg = snapshotItem.Message || snapshotItem.message;
    if (msg) row._body.replaceChildren(recordBody({ reasoning: msg.reasoning, content: msg.content }));
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
      const cls = frame.t === "reasoning_delta" ? "reasoning" : "";
      const line = document.createElement("div");
      if (cls) line.className = cls;
      line.appendChild(textNode(frame.text));
      row._body.appendChild(line);
      scrollToBottom();
      break;
    }
    case "tool_started": {
      const row = state.byItem.get(frame.item);
      if (!row) {
        const r = { t: "tool.result", item: frame.item, ts: "" };
        const toolRow = rowFor(r, true);
        rowsEl.appendChild(toolRow);
        state.byItem.set(frame.item, toolRow);
      }
      break;
    }
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

document.getElementById("load-more").addEventListener("click", loadHistory);
logEl.addEventListener("scroll", () => {
  if (logEl.scrollTop < 60 && state.more) loadHistory();
});

let loading = false;
async function loadHistory() {
  if (loading || !state.more) return;
  loading = true;
  try {
    const anchor = await anchorOfFirstHeld();
    const res = await fetch("/api/history?before=" + encodeURIComponent(anchor) + "&limit=50");
    if (res.status === 409) {
      // Stale anchor (server restarted): resynchronize from scratch.
      state.reconnectFrom = "";
      state.es.close();
      connect();
      return;
    }
    const data = await res.json();
    // Backward verification: fold(anchor, records) must equal our current
    // first-held digest — both ends known, stronger than forward.
    let h = data.anchor;
    for (const r of data.records) {
      h = await sha256Hex(h + "\n" + chainInput(r));
    }
    if (h !== anchor) {
      setSync(false, "");
      state.more = false;
      document.getElementById("load-more").hidden = true;
      return;
    }
    // Prepend in order, keeping scroll anchored.
    const prevHeight = logEl.scrollHeight;
    for (const r of data.records) {
      state.records.unshift(r);
      const row = rowFor(r, false);
      row._body.appendChild(recordBody(r));
      if (rowsEl.firstChild) rowsEl.insertBefore(row, rowsEl.firstChild);
      else rowsEl.appendChild(row);
      state.byItem.set(r.item, row);
    }
    state.more = !!data.more;
    document.getElementById("load-more").hidden = !state.more;
    logEl.scrollTop += logEl.scrollHeight - prevHeight;
  } finally {
    loading = false;
  }
}

async function anchorOfFirstHeld() {
  if (!state.records.length) return await sha256Hex(chainVersion + sessionIdFromUrl());
  return await digestAfterN(state.records.length); // head; first-held for window loads
}

async function digestAfterN(n) {
  let h = await sha256Hex(chainVersion + sessionIdFromUrl());
  for (let i = 0; i < n; i++) h = await sha256Hex(h + "\n" + chainInput(state.records[i]));
  return h;
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

// Refresh the status area from /api/state when the stream is down
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

state.head = "init";
(async () => {
  // The genesis must come from the served session id, not the URL guess.
  try {
    const res = await fetch("/api/state");
    if (res.ok) {
      const s = await res.json();
      state.session = s.session;
      history.replaceState(null, "", "?session=" + s.session);
      state.head = await sha256Hex(chainVersion + s.session);
    } else {
      state.head = await sha256Hex(chainVersion + sessionIdFromUrl());
    }
  } catch (_) {
    state.head = await sha256Hex(chainVersion + sessionIdFromUrl());
  }
  connect();
})();
