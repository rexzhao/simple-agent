// sai web log view. The page's contract (docs/webui.md):
//   page store == anchor prefix (server-attested) ∪ received record stream
// verified continuously by recomputing the digest chain over what the page
// holds and comparing it against the server's head in every status frame.
//
// Multi-session (docs/multi-session.md §6): the route is the hash
// (#/s/<id>); switching sessions is a FULL state reset behind a
// generation guard — in-flight frames of the old stream WILL arrive
// after the switch (every switch, not a rare race), and the per-session
// cache lets a switch back resume from the held head instead of
// replaying.
"use strict";

import { chainStep } from "./chain.mjs";
import { initialState, initChain, applySnapshot, applyRecord, applyStatus, applyHistory } from "./reducer.mjs";

// ---------- session views ----------

// `state` is the active session's reducer state. A switch replaces it
// wholesale; async work captures the object it started with and drops
// its effects when the object is no longer current — that identity
// check IS the generation guard (the awaits inside chain folds are
// exactly where an old stream's frame would land on the new session).
let state = initialState();
let es = null;

// freshState builds a session view's state: the reducer fields plus the
// page's own (identity-keyed row map, resume cursor, streaming slot).
function freshState() {
  return Object.assign(initialState(), {
    byItem: new Map(),  // itemID -> row element (committed + in-flight)
    reconnectFrom: "",
    streaming: null,    // {item} of the in-flight assistant message
  });
}

// The per-session cache: what a switch-away preserves so a switch back
// can resume from the held head (?from=<head>) instead of replaying.
// Keyed by session id — never one global blob. Record objects are
// immutable JSON, so a shallow array copy is a complete snapshot.
const sessionCache = new Map();

function saveToCache() {
  if (state.session && state.head) {
    sessionCache.set(state.session, {
      records: state.records.slice(),
      head: state.head,
      prefixAnchor: state.prefixAnchor,
      more: state.more,
    });
  }
}

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

// api builds a session-scoped route for the ACTIVE session. Callers
// that captured a state object must not use it after a switch — the
// identity checks below are the guard.
function api(suffix) { return "/api/s/" + state.session + suffix; }

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
    // The class is addressable state: steer-row editing finds the
    // current text through it (querySelector(".content")).
    div.className = "content";
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
    // the same item — including a queued steer row becoming the real
    // user input.
    const existing = state.byItem.get(r.item);
    existing._body.replaceChildren(recordBody(r));
    existing.classList.remove("streaming", "steer-queued");
    existing.querySelectorAll(".steer-controls").forEach(el => el.remove());
    existing._reasoningEl = null;
    existing._contentEl = null;
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
  const from = state.reconnectFrom;
  const url = api("/stream") + (from ? "?from=" + encodeURIComponent(from) : "");
  const myState = state;
  const source = new EventSource(url);
  es = source;
  source.onmessage = e => {
    let frame;
    try {
      frame = JSON.parse(e.data);
    } catch (err) {
      console.error("sai page: bad frame", err);
      return;
    }
    enqueue(() => dispatch(frame, myState));
  };
  source.onerror = () => {
    // EventSource retries on its own; the in-memory digest lets the
    // retry resume with ?from= instead of replaying.
    if (myState === state) myState.reconnectFrom = myState.head;
  };
}

async function dispatch(frame, st) {
  // The generation guard, checked after every await below: an old
  // session's frame must never touch the new session's state or DOM.
  if (st !== state) return;
  switch (frame.kind) {
    case "snapshot": {
      const fx = await applySnapshot(st, frame);
      if (st !== state) return;
      if (fx.reset) {
        st.byItem = new Map();
        rowsEl.replaceChildren();
      }
      for (const r of fx.appended) appendRecord(r);
      scrollToBottom();
      st.reconnectFrom = "";
      document.getElementById("load-more").hidden = !st.more;
      if (frame.streaming) beginStreamingItem(frame.streaming.item, frame.streaming);
      if (frame.tool) beginToolItem(frame.tool.item, frame.tool.name, frame.tool.args);
      // The authoritative queue: drop steer rows that are no longer
      // queued (withdrawn while this page was disconnected — the record
      // that would have merged them never comes).
      {
        const queuedIDs = new Set((frame.steered || []).map(e => e.item));
        for (const [id, row] of st.byItem) {
          if (row.classList.contains("steer-queued") && !queuedIDs.has(id)) {
            row.remove();
            st.byItem.delete(id);
          }
        }
        for (const entry of frame.steered || []) beginSteerRow(entry.item, entry.text);
      }
      updateStatus();
      if (frame.d) setSync(st.head === frame.d, frame.d);
      break;
    }
    case "record":
      await applyRecord(st, frame.r);
      if (st !== state) return;
      appendRecord(frame.r);
      scrollToBottom();
      break;
    case "event":
      handleEvent(frame);
      break;
    case "status":
      applyStatus(st, frame);
      updateStatus();
      if (frame.notice) showNotice(frame.notice);
      if (frame.d) setSync(st.head === frame.d, frame.d);
      break;
  }
}

function updateStatus() {
  document.getElementById("session").textContent = state.session ? "session " + state.session : "no session";
  document.getElementById("count").textContent = state.session ? state.records.length + " records" + (state.more ? "+" : "") : "";
  const run = document.getElementById("runstate");
  run.textContent = state.running ? "running" : "idle";
  run.className = state.running ? "running" : "idle";
  const composer = document.getElementById("composer");
  const active = !!state.session;
  document.getElementById("input").disabled = !active;
  document.getElementById("compact").disabled = !active;
  document.getElementById("stop").disabled = !active;
  // Steer keeps the composer usable while running.
  const send = document.getElementById("send");
  send.textContent = state.running ? "steer" : "send";
  send.disabled = !active;
  document.getElementById("input").placeholder = !active ? "select a session…"
    : state.running ? "steer a message…" : "message…";
  renderSidebarCurrent();
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
    row._reasoningEl = null;
    row._contentEl = null;
  }
  return row;
}

// ensureStreamText returns the accumulating container for one kind of
// streaming text on a row, creating it on first use. The reasoning
// container stays before the content container, matching the committed
// record's render order.
function ensureStreamText(row, reasoning) {
  if (reasoning) {
    if (!row._reasoningEl) {
      const div = document.createElement("div");
      div.className = "reasoning";
      if (row._contentEl) row._body.insertBefore(div, row._contentEl);
      else row._body.appendChild(div);
      row._reasoningEl = div;
    }
    return row._reasoningEl;
  }
  if (!row._contentEl) {
    const div = document.createElement("div");
    div.className = "content";
    row._body.appendChild(div);
    row._contentEl = div;
  }
  return row._contentEl;
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
    row._reasoningEl = null;
    row._contentEl = null;
  }
  return row;
}

// ---------- queued steer messages (addressable by identity) ----------

// A queued steer row merges into the real user.input row when the
// record lands — same ItemID, same merge rule as in-flight items.
function beginSteerRow(itemID, text) {
  let row = state.byItem.get(itemID);
  if (!row) {
    const r = { t: "user.input", item: itemID, ts: "", content: text };
    row = rowFor(r, true);
    row.classList.add("steer-queued");
    row._body.appendChild(recordBody({ content: text }));
    appendSteerControls(row, itemID);
    rowsEl.appendChild(row);
    state.byItem.set(itemID, row);
    scrollToBottom();
  }
  return row;
}

function appendSteerControls(row, itemID) {
  const controls = document.createElement("span");
  controls.className = "steer-controls";
  const edit = document.createElement("button");
  edit.textContent = "edit";
  edit.onclick = () => editSteer(itemID, row);
  const drop = document.createElement("button");
  drop.textContent = "withdraw";
  drop.onclick = () => withdrawSteer(itemID);
  controls.append(edit, drop);
  row.appendChild(controls);
}

function editSteer(itemID, row) {
  const current = row._body.querySelector(".content")?.textContent || "";
  const next = prompt("Edit steer message", current);
  if (next === null || !next.trim()) return;
  fetch(api("/steer"), {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ item: itemID, text: next }),
  }).then(res => {
    if (!res.ok) showError("steer edit failed");
  });
}

function withdrawSteer(itemID) {
  fetch(api("/steer?item=" + encodeURIComponent(itemID)), { method: "DELETE" })
    .then(res => {
      if (!res.ok) showError("steer withdraw failed");
    });
}

function handleEvent(frame) {
  switch (frame.t) {
    case "steer_queued":
      beginSteerRow(frame.item, frame.text);
      break;
    case "steer_updated": {
      const row = state.byItem.get(frame.item);
      if (row && row.classList.contains("steer-queued")) {
        row._body.replaceChildren(recordBody({ content: frame.text }));
        appendSteerControls(row, frame.item);
      }
      break;
    }
    case "steer_withdrawn": {
      const row = state.byItem.get(frame.item);
      if (row && row.classList.contains("steer-queued")) {
        row.remove();
        state.byItem.delete(frame.item);
      }
      break;
    }
    case "assistant_message_started":
      state.streaming = { item: frame.item };
      beginStreamingItem(frame.item);
      break;
    case "reasoning_delta":
    case "text_delta": {
      if (!state.streaming) break;
      const row = state.byItem.get(state.streaming.item);
      if (!row) break;
      // Deltas accumulate into one container per kind — a div per delta
      // would put every fragment on its own line.
      const container = ensureStreamText(row, frame.t === "reasoning_delta");
      container.appendChild(textNode(frame.text));
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
  const st = state;
  const res = await fetch(api("/history?before=" + encodeURIComponent(st.prefixAnchor) + "&limit=50"));
  if (st !== state) return;
  if (res.status === 409) {
    // Stale anchor (server restarted): resynchronize from scratch.
    st.reconnectFrom = "";
    if (es) es.close();
    connect();
    return;
  }
  const data = await res.json();
  if (st !== state) return;
  // Backward verification and the store/cursor update live in the
  // reducer (shared with the fixture test); the head never changes —
  // history extension cannot alter a running hash's tail.
  const outcome = await applyHistory(st, data);
  if (st !== state) return;
  if (outcome !== "ok") {
    setSync(false, "");
    st.more = false;
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
    st.byItem.set(r.item, row);
  }
  document.getElementById("load-more").hidden = !st.more;
  logEl.scrollTop += logEl.scrollHeight - prevHeight;
}

// ---------- composer ----------

// While a run is active the composer steers (queued for the next model
// request); each direction races the run state, so both 409s fall back
// to the other endpoint.
document.getElementById("composer").addEventListener("submit", async e => {
  e.preventDefault();
  if (!state.session) return;
  const input = document.getElementById("input");
  const text = input.value.trim();
  if (!text) return;
  // The interaction guard: capture the target session and build every
  // URL from the CAPTURED id — a switch mid-flight must not redirect
  // the fallback POST (and this text) to the newly-viewed session.
  // The 409 fallback itself is load-bearing: a cache-restored view
  // holds running=false until the resume snapshot corrects it, so the
  // first POST of a restored-running session races as the wrong kind.
  const st = state;
  const target = "/api/s/" + state.session;
  const send = () => postJSON2(target + "/send", { text });
  const steer = () => postJSON2(target + "/steer", { text });
  let res = st.running ? await steer() : await send();
  if (st !== state) return;
  if (res.status === 409) {
    const fallback = st.running ? send : steer;
    res = await fallback();
    if (st !== state) return;
  }
  if (res.ok) {
    input.value = "";
  } else {
    const text = await res.text();
    if (st !== state) return;
    showError(text);
  }
});

async function postJSON2(path, body) {
  return fetch(path, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
}

document.getElementById("stop").addEventListener("click", () => {
  if (!state.session) return;
  fetch(api("/stop"), { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" });
});

document.getElementById("compact").addEventListener("click", async () => {
  if (!state.session) return;
  const st = state;
  const res = await fetch(api("/compact"), { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" });
  if (st !== state) return;
  if (res.status === 409) {
    const data = await res.json().catch(() => ({}));
    if (st !== state) return;
    showError(data.error === "nothing_to_compact" ? "nothing to compact" : "busy");
  }
});

// ---------- sidebar (project/session list) ----------

// `active` comes from /api/projects (TTL-cached server-side) — it means
// "open in this serve process", NOT "running". The running chip is only
// ever shown for the session this page is viewing, from its live
// stream state.
let projectsBody = { projects: [], registry_ok: true };
let sidebarSignature = "";

async function refreshProjects() {
  try {
    const res = await fetch("/api/projects");
    if (!res.ok) return;
    const body = await res.json();
    projectsBody = body;
    renderSidebar();
  } catch (_) { /* serve away; the next tick retries */ }
}

function renderSidebar() {
  // Signature skip: the sidebar is a pure derivation of the projects
  // payload and the current session's id/running — rebuilding an
  // unchanged derivation would drop keyboard focus and hover every
  // status transition and every 5s poll (the links are focusable).
  const signature = JSON.stringify(projectsBody) + "|" + state.session + "|" + !!state.running;
  if (signature === sidebarSignature) return;
  sidebarSignature = signature;
  const host = document.getElementById("projects");
  host.replaceChildren();
  for (const p of projectsBody.projects || []) {
    const block = document.createElement("div");
    block.className = "project";
    const path = document.createElement("div");
    path.className = "project-path";
    path.appendChild(textNode(p.path));
    path.title = p.id;
    block.appendChild(path);
    for (const s of p.sessions || []) {
      // The row is a div (a button inside an <a> is invalid HTML); the
      // navigation target is the inner link wrapping title and meta.
      const row = document.createElement("div");
      row.className = "session-link";
      const link = document.createElement("a");
      link.href = "#/s/" + s.id;
      if (s.id === state.session) row.classList.add("current");
      const title = document.createElement("span");
      title.className = "title";
      title.appendChild(textNode(s.title || s.id));
      const meta = document.createElement("span");
      meta.className = "meta";
      const when = document.createElement("span");
      when.appendChild(textNode((s.created || "").slice(0, 10)));
      meta.appendChild(when);
      if (s.active) {
        const badge = document.createElement("span");
        badge.className = "badge";
        badge.appendChild(textNode("open"));
        meta.appendChild(badge);
      }
      // The running chip: only from the live view, never from `active`.
      if (s.id === state.session && state.running) {
        const badge = document.createElement("span");
        badge.className = "badge running";
        badge.appendChild(textNode("running"));
        meta.appendChild(badge);
      }
      link.append(title, meta);
      row.appendChild(link);
      // The delete affordance: one confirmation, then DELETE. A session
      // this page is viewing must be left first — its own stream holds
      // the server-side refs (session_open); navigate away, wait for
      // the stream to actually close, then delete.
      const del = document.createElement("button");
      del.className = "delete";
      del.textContent = "✕";
      del.title = "delete session";
      del.onclick = async e => {
        e.preventDefault();
        e.stopPropagation();
        if (!confirm("Delete this session and all its records?")) return;
        // Re-entry guard: the retry loop spans seconds. Every exit
        // re-enables the button; the identity guard lives HERE, inside
        // done — one discipline for all exits.
        del.disabled = true;
        const done = text => {
          del.disabled = false;
          if (text !== undefined && st === state) showError(text);
        };
        if (s.id === state.session) {
          // Our own view holds the server-side refs: navigate away and
          // let the hashchange task tear the stream down. The readyState
          // wait below only waits out that task; the server releases
          // refs when it notices the connection drop, so the real
          // convergence is the session_open retry loop.
          const closing = es;
          location.hash = "";
          for (let i = 0; i < 40 && closing && closing.readyState !== 2; i++) {
            await new Promise(res => setTimeout(res, 50));
          }
        }
        // The identity is captured AFTER the self-navigation above: for
        // the delete-own-view flow the state object has already been
        // replaced by the list view, and capturing earlier would swallow
        // every failure toast of this exact flow (a destructive
        // operation failing silently is the worst outcome). From here,
        // st !== state means the user switched away mid-delete — the
        // one case worth suppressing.
        const st = state;
        try {
          // A response body is single-use: read it as TEXT once and
          // parse locally — the retry loop and the final branch share
          // the same parse, never re-reading a consumed body.
          // session_open (a view still holds it) and session_closing
          // (the sweeper or a prior delete is tearing it down) converge
          // with bounded retries; the others need a human action.
          let res, raw = "", code = "";
          const read = async r => {
            raw = await r.text().catch(() => "");
            try { code = (JSON.parse(raw) || {}).error || ""; } catch (_) { code = ""; }
          };
          for (let attempt = 0; ; attempt++) {
            res = await fetch("/api/s/" + s.id, { method: "DELETE" });
            if (res.status !== 409 && res.status !== 503) break;
            await read(res);
            if (code !== "session_open" && code !== "session_closing") break;
            if (attempt >= (code === "session_open" ? 5 : 10)) {
              done(code === "session_open"
                ? "session is open in a view — close it first"
                : "session is busy, try again");
              return;
            }
            await new Promise(r => setTimeout(r, 200));
          }
          if (res.ok || res.status === 404) {
            // Deleted, or already gone — either way it is a server-side
            // fact: drop the cache and refresh regardless of which view
            // is current now.
            sessionCache.delete(s.id);
            refreshProjects();
            done();
            return;
          }
          // 409/503 bodies were read (and parsed) inside the loop; only
          // the other statuses (500 …) still have an unread body here.
          if (res.status !== 409 && res.status !== 503) {
            raw = await res.text().catch(() => "");
            try { code = (JSON.parse(raw) || {}).error || ""; } catch (_) { code = ""; }
          }
          const text = ({
            run_active: "stop the run first",
            session_locked: "session is locked by another process",
            session_open: "session is open in a view — close it first",
            session_closing: "session is busy, try again",
          })[code] || raw || "delete failed";
          done(text);
        } catch (_) {
          done("delete failed");
        }
      };
      row.appendChild(del);
      block.appendChild(row);
    }
    host.appendChild(block);
  }
}

function renderSidebarCurrent() {
  // Re-derive: the signature skip inside renderSidebar makes this a
  // no-op unless the current/running markers actually changed.
  renderSidebar();
}

setInterval(refreshProjects, 5000);

// ---------- routing ----------

// The hash IS the route: #/s/<id> opens a session, anything else is the
// list landing. hashchange covers back/forward; list clicks write the
// hash; unknown ids are presented and sent back to the list.
function route() {
  const m = location.hash.match(/^#\/s\/([a-z0-9]+)$/);
  if (m) {
    openSession(m[1]);
  } else {
    showList();
  }
}

window.addEventListener("hashchange", route);

function teardownSession() {
  saveToCache();
  if (es) {
    es.close();
    es = null;
  }
}

function showList() {
  teardownSession();
  state = freshState();
  rowsEl.replaceChildren();
  document.getElementById("load-more").hidden = true;
  document.getElementById("notices").textContent = "";
  const sync = document.getElementById("sync");
  sync.className = "sync-unknown";
  sync.textContent = "…";
  updateStatus();
}

async function openSession(id, retries = 0) {
  if (state.session === id && es) return; // already viewing
  teardownSession();
  state = freshState();
  state.session = id;
  rowsEl.replaceChildren();
  document.getElementById("load-more").hidden = true;
  document.getElementById("notices").textContent = "";
  const sync = document.getElementById("sync");
  sync.className = "sync-unknown";
  sync.textContent = "…";

  // Hydrate from the per-session cache: the rows re-render locally and
  // the stream resumes from the held head. A miss simply replays.
  const cached = sessionCache.get(id);
  if (cached) {
    state.records = cached.records.slice();
    state.head = cached.head;
    state.prefixAnchor = cached.prefixAnchor;
    state.more = cached.more;
    state.reconnectFrom = cached.head;
    for (const r of state.records) appendRecord(r);
    scrollToBottom();
    document.getElementById("load-more").hidden = !state.more;
  } else {
    await initChain(state, id);
    state.reconnectFrom = "";
  }

  // Probe before connecting: EventSource cannot read status codes, so
  // the distinguishable failures are presented here (and only 503 is
  // retried, by contract).
  const st = state;
  let res;
  try {
    res = await fetch(api("/state"));
  } catch (_) {
    showError("serve unreachable");
    return;
  }
  if (st !== state) return;
  if (res.status === 404) {
    sessionCache.delete(id);
    // Detach before the hash change routes to the list: the list's
    // teardown would otherwise re-cache this session as a ghost
    // (empty records + genesis head) right after the delete.
    st.session = "";
    showError("session not found");
    if (location.hash) location.hash = "";
    route();
    return;
  }
  if (res.status === 409) {
    const data = await res.json().catch(() => ({}));
    if (st !== state) return;
    showError(data.error === "session_locked" ? "session is locked by another process" : "conflict");
    return;
  }
  if (res.status === 503) {
    // Our own eviction teardown: retry shortly, bounded — a session
    // stuck in closing must surface as an error, not an endless toast
    // loop.
    if (retries >= 4) {
      showError("session is unavailable (closing did not finish)");
      return;
    }
    showError("session is closing, retrying…");
    setTimeout(() => { if (st === state && state.session === id) openSession(id, retries + 1); }, 1000);
    return;
  }
  if (!res.ok) {
    const text = await res.text();
    if (st !== state) return;
    showError(text);
    return;
  }
  if (st !== state) return;
  updateStatus();
  connect();
}

// ---------- boot ----------

// Refresh the status area from /api/state while the stream is down
// (covers the gap between connects).
setInterval(async () => {
  if (state.session && es && es.readyState === 2) {
    const st = state;
    try {
      const res = await fetch(api("/state"));
      if (st !== state) return;
      if (res.ok) {
        const s = await res.json();
        if (st !== state) return;
        st.running = s.running;
        updateStatus();
      }
    } catch (_) { /* server away */ }
  }
}, 2000);

route();
refreshProjects();
