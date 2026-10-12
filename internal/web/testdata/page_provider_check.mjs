// The page-as-provider protocol check: drives PageProvider (the chat
// client's broker debug surface, TS via node type stripping) with a
// fake WebSocket, asserting the provider protocol shapes —
// registration, session-keyed re-registration, tool/describe,
// tool/call dispatch and validation, notification tolerance.
//
// Live mode (optional): SAI_LIVE_BROKER=ws://host:port/hub/provider
// runs the full loop against a real broker — the page provider
// registers, and an MCP-host flow over POST /mcp discovers it and
// calls page_state through execute_provider_tool.
//
// Run by TestPageProviderFixture via `node page_provider_check.mjs`.
import { PageProvider } from "../../../clients/chat/src/pageProvider.ts";

const failures = [];
function assert(cond, what) {
  if (!cond) failures.push(what);
}

// A fake broker socket: the check is the broker side.
class FakeWS {
  constructor() {
    this.sent = [];
    this.open = false;
    this.closed = false;
    this.onopen = null;
    this.onmessage = null;
    this.onclose = null;
    this.onerror = null;
  }
  send(data) {
    if (!this.closed) this.sent.push(JSON.parse(data));
  }
  close() {
    this.closed = true;
    this.onclose?.();
  }
  // Broker-side helpers.
  serverOpen() {
    this.open = true;
    this.onopen?.();
  }
  request(method, params) {
    this.onmessage?.({ data: JSON.stringify({ jsonrpc: "2.0", id: "q-" + this.sent.length, method, params }) });
    const frame = this.sent[this.sent.length - 1];
    return frame;
  }
  notify(method, params) {
    this.onmessage?.({ data: JSON.stringify({ jsonrpc: "2.0", method, params }) });
  }
}

const sockets = [];
const provider = new PageProvider(() => {
  const ws = new FakeWS();
  sockets.push(ws);
  return ws;
});

// ---- 1. registration on open ----
provider.start("ws://broker.test/hub/provider");
assert(sockets.length === 1, "s1 one socket");
const ws1 = sockets[0];
ws1.serverOpen();
const reg1 = ws1.sent.find(m => m.method === "provider/register");
assert(!!reg1, "s1 registration sent");
assert(reg1.params.protocol_version === 2, "s1 protocol_version 2");
assert(reg1.params.provider_id === "sai-chat-list", "s1 provider id (list)");
assert(reg1.params.visibility.allow_other_ips === true, "s1 allow_other_ips");
assert(reg1.params.manifest.capabilities.tools === true, "s1 tools capability");
assert(
  ["page_state", "page_sessions", "page_select_session", "page_read_conversation", "page_send", "page_text"].every(n =>
    reg1.params.manifest.tools.some(t => t.name === n),
  ),
  "s1 all six tools in manifest",
);

// ---- 2. session switch re-registers under sai-chat-<id> ----
provider.setSession("sess-abc");
assert(sockets.length === 2, "s2 reconnect on session switch");
assert(ws1.closed, "s2 old socket closed");
const ws2 = sockets[1];
ws2.serverOpen();
const reg2 = ws2.sent.find(m => m.method === "provider/register");
assert(reg2.params.provider_id === "sai-chat-sess-abc", "s2 provider id keyed by session");

// ---- 3. tool/describe ----
const desc = ws2.request("tool/describe", { name: "page_send" });
assert(desc.result.read_only === false, "s3 page_send not read_only");
assert(!!desc.result.input_schema.properties.text, "s3 input schema");
const descRO = ws2.request("tool/describe", { name: "page_state" });
assert(descRO.result.read_only === true, "s3 page_state read_only");
const descUnknown = ws2.request("tool/describe", { name: "nope" });
assert(!!descUnknown.result.error, "s3 unknown tool describe errors");

// ---- 4. tool/call with commands wired ----
provider.commands = {
  state: () => ({ session: "sess-abc", running: false }),
  sessions: () => [{ id: "sess-abc", title: "t" }],
  selectSession: id => {
    provider.commands = { ...provider.commands, _selected: id } && provider.commands;
    selected = id;
  },
  readConversation: () => [{ role: "user", parts: [] }],
  send: () => ({ ok: true }),
  text: () => "page text",
};
let selected = "";
const callState = ws2.request("tool/call", { name: "page_state", arguments: {} });
assert(callState.result.status === "success" && callState.result.content.session === "sess-abc", "s4 page_state");
const callSelect = ws2.request("tool/call", { name: "page_select_session", arguments: { session_id: "sess-xyz" } });
assert(callSelect.result.status === "success" && selected === "sess-xyz", "s4 select dispatch");
const callSend = ws2.request("tool/call", { name: "page_send", arguments: { text: "hello" } });
assert(callSend.result.status === "success", "s4 send");
const callRead = ws2.request("tool/call", { name: "page_read_conversation", arguments: {} });
assert(callRead.result.content[0].role === "user", "s4 read_conversation");

// ---- 5. validation and unknown tools ----
const sendNoText = ws2.request("tool/call", { name: "page_send", arguments: {} });
assert(sendNoText.result.status === "error", "s5 send without text errors");
const unknown = ws2.request("tool/call", { name: "nope", arguments: {} });
assert(unknown.result.status === "error", "s5 unknown tool errors");

// ---- 6. notifications (no id) get no response ----
const before = ws2.sent.length;
ws2.notify("server/welcome", { hello: true });
assert(ws2.sent.length === before, "s6 notification ignored");

// ---- 7. no commands wired ----
provider.commands = null;
const notReady = ws2.request("tool/call", { name: "page_state", arguments: {} });
assert(notReady.result.status === "error" && /not ready/.test(notReady.result.error), "s7 not ready");

// ---- 8. provider/getManifest ----
const gm = ws2.request("provider/getManifest", {});
assert(gm.result.manifest_version === 1, "s8 getManifest");

if (failures.length) {
  console.error("page_provider_check: FAILED");
  for (const f of failures) console.error("  - " + f);
  process.exit(1);
}
console.log("page_provider_check: OK (8 scenarios)");

// ---- live mode: the full loop against a real broker ----
const live = process.env.SAI_LIVE_BROKER;
if (live) {
  const httpBase = live.replace(/^ws/, "http").replace(/\/hub\/provider$/, "");
  const liveProvider = new PageProvider();
  liveProvider.commands = {
    state: () => ({ session: "live-probe", marker: "sai-page-provider-live-check" }),
    sessions: () => [],
    selectSession: () => {},
    readConversation: () => [],
    send: () => ({ ok: false, error: "live check: send disabled" }),
    text: () => "sai page provider live check",
  };
  liveProvider.setSession("live-probe");
  liveProvider.start(live);

  const sleep = ms => new Promise(r => setTimeout(r, ms));
  await sleep(1500); // register

  // MCP host flow: initialize -> initialized -> execute_provider_tool.
  let sessionId = null;
  const mcp = async (method, params) => {
    const res = await fetch(httpBase + "/mcp", {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Accept: "application/json, text/event-stream",
        ...(sessionId ? { "Mcp-Session-Id": sessionId } : {}),
      },
      body: JSON.stringify({ jsonrpc: "2.0", id: "live-" + Math.random(), method, params }),
    });
    const sid = res.headers.get("Mcp-Session-Id");
    if (sid) sessionId = sid;
    const ct = res.headers.get("Content-Type") || "";
    const body = await res.text();
    if (ct.includes("text/event-stream")) {
      const line = body.split("\n").find(l => l.startsWith("data: "));
      return line ? JSON.parse(line.slice(6)) : null;
    }
    return body ? JSON.parse(body) : null;
  };

  const init = await mcp("initialize", {
    protocolVersion: "2025-06-18",
    capabilities: {},
    clientInfo: { name: "sai-page-provider-check", version: "0" },
  });
  assert(init?.result?.capabilities?.tools, "live: initialize");
  await fetch(httpBase + "/mcp", {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      Accept: "application/json, text/event-stream",
      ...(sessionId ? { "Mcp-Session-Id": sessionId } : {}),
    },
    body: JSON.stringify({ jsonrpc: "2.0", method: "notifications/initialized" }),
  });
  const call = await mcp("tools/call", {
    name: "execute_provider_tool",
    arguments: { provider_id: "sai-chat-live-probe", tool_name: "page_state", arguments: {} },
  });
  const text = call?.result?.content?.[0]?.text || "";
  assert(/sai-page-provider-live-check/.test(text), "live: page_state round trip: " + text.slice(0, 200));
  liveProvider.stop();

  if (failures.length) {
    console.error("page_provider_check: LIVE FAILED");
    for (const f of failures) console.error("  - " + f);
    process.exit(1);
  }
  console.log("page_provider_check: LIVE OK (registered, called via execute_provider_tool)");
}
