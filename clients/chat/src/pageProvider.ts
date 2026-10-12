// The page as a broker provider: the agent's MCP debug flow (through
// mcp-provider-broker) discovers this page and operates it. The
// connection is the PROVIDER WebSocket (ws://<broker>/hub/provider,
// JSON-RPC 2.0) — WebSocket is not subject to CORS, so the page
// connects directly; the serve side has no part in this.
//
// Identity: one provider per page, keyed by the session it is viewing
// (sai-chat-<session id>, "list" when none) — the agent in session X
// addresses the page showing X deterministically, no discovery dance.
// Switching sessions re-registers under the new id (the broker drops
// the old registration when the socket closes). Duplicate-id semantics
// (verified live): an active registration is NOT displaced by a newer
// one; the newer takes over only after the older disconnects. The id
// carries no credential — anyone who can reach the broker could claim
// it during the page's reconnect gaps. The broker is an unauthenticated
// LAN debug surface; the page provider inherits that same trust class.
//
// Enabling (three doors, in order of user intent):
//   1. window.sai.pageProvider(url, {send}) — explicit DevTools action:
//      persists to localStorage, auto-connects on later loads.
//   2. ?page-provider=<ws-url> — NOT persisted, NOT auto-connecting: a
//      crafted link must not silently wire the page to a foreign
//      broker (page_send drives the real composer, which reaches the
//      agent's shell tools). The page shows a confirmation banner; a
//      confirmed link is session-only.
//   3. window.sai.pageProviderOff() — stop + clear the stored config.
// Only loopback/private-network brokers are accepted as targets.
// page_send is opt-in SEPARATELY (it upgrades read-only debugging to
// driving execution).

export type SendResult = { ok: boolean; confirmed?: boolean; error?: string };

export type PageCommands = {
  state(): Record<string, unknown>;
  sessions(): unknown[];
  selectSession(id: string): void;
  readConversation(): unknown[];
  send(text: string): Promise<SendResult>;
  text(): string;
};

type ToolEntry = { name: string; summary: string; group: string };

const TOOLS: ToolEntry[] = [
  { name: "page_state", summary: "The page's current view state: session, connection, run state, chain verification, steer queue, URL.", group: "page" },
  { name: "page_sessions", summary: "The sessions the page lists (the projects poll view).", group: "page" },
  { name: "page_select_session", summary: "Switch the page to a session by id (the real route switch).", group: "page" },
  { name: "page_read_conversation", summary: "The currently rendered conversation (roles and parts) — what the human sees.", group: "page" },
  {
    name: "page_send",
    summary:
      "Type a message into the composer and click send — the real UI path, not the API. Dispatched, not confirmed: " +
      "the result carries whether the composer cleared within a bound. Opt-in on the page (it drives execution); " +
      "disabled pages answer with the enabling instructions.",
    group: "page",
  },
  { name: "page_text", summary: "document.body.innerText — a full-page text snapshot.", group: "page" },
];

const INPUT_SCHEMAS: Record<string, object> = {
  page_state: { type: "object", properties: {} },
  page_sessions: { type: "object", properties: {} },
  page_select_session: {
    type: "object",
    properties: { session_id: { type: "string" } },
    required: ["session_id"],
  },
  page_read_conversation: { type: "object", properties: {} },
  page_send: {
    type: "object",
    properties: { text: { type: "string" } },
    required: ["text"],
  },
  page_text: { type: "object", properties: {} },
};

const MANIFEST = {
  manifest_version: 1,
  capabilities: {
    tools: true,
    resources: false,
    tool_describe: true,
    resource_read: false,
  },
  tools: TOOLS,
  resources: [],
};

// The WebSocket constructor is injectable so the fixture check can
// drive the protocol without a network.
export type WebSocketLike = {
  send(data: string): void;
  close(): void;
  onopen: (() => void) | null;
  onmessage: ((ev: { data: string }) => void) | null;
  onclose: (() => void) | null;
  onerror: (() => void) | null;
};

export class PageProvider {
  private url = "";
  private wsFactory: (url: string) => WebSocketLike;
  private ws: WebSocketLike | null = null;
  private stopped = true;
  private session = "";
  private retryTimer: ReturnType<typeof setTimeout> | null = null;
  private retryDelay = 250;
  private reqID = 0;

  commands: PageCommands | null = null;
  // page_send drives the real composer, which reaches the agent's
  // shell tools — it is opt-in, separately from connecting.
  sendEnabled = false;

  constructor(wsFactory?: (url: string) => WebSocketLike) {
    this.wsFactory = wsFactory ?? ((url: string) => new WebSocket(url) as unknown as WebSocketLike);
  }

  start(url: string) {
    this.url = url;
    this.stopped = false;
    this.connect();
  }

  stop() {
    this.stopped = true;
    if (this.retryTimer !== null) clearTimeout(this.retryTimer);
    this.retryTimer = null;
    this.retryDelay = 250;
    this.ws?.close();
    this.ws = null;
  }

  get connected() {
    return !this.stopped && this.ws !== null;
  }

  // setSession re-registers under sai-chat-<session>: the provider id
  // IS the addressing contract.
  setSession(id: string) {
    if (id === this.session) return;
    this.session = id;
    if (!this.stopped) {
      // Reconnect under the new id; the broker drops the old
      // registration when the old socket closes.
      this.ws?.close();
      this.ws = null;
      this.connect();
    }
  }

  private connect() {
    if (this.stopped || !this.url) return;
    const ws = this.wsFactory(this.url);
    this.ws = ws;
    ws.onopen = () => {
      this.retryDelay = 250;
      this.send({
        jsonrpc: "2.0",
        id: "reg-" + ++this.reqID,
        method: "provider/register",
        params: {
          protocol_version: 2,
          provider_type: "sai-chat",
          provider_id: "sai-chat-" + (this.session || "list"),
          provider_name: "sai chat",
          visibility: { allow_other_ips: true },
          manifest: MANIFEST,
        },
      });
    };
    ws.onmessage = ev => this.handle(ev.data);
    ws.onclose = () => {
      if (this.stopped) return;
      this.retryTimer = setTimeout(() => {
        this.retryDelay = Math.min(this.retryDelay * 2, 5000);
        this.connect();
      }, this.retryDelay);
    };
    ws.onerror = () => {
      /* close follows; the reconnect lives in onclose */
    };
  }

  private send(msg: unknown) {
    try {
      this.ws?.send(JSON.stringify(msg));
    } catch {
      /* socket mid-close */
    }
  }

  private handle(raw: string) {
    let msg: { id?: string | number; method?: string; params?: Record<string, unknown> };
    try {
      msg = JSON.parse(raw);
    } catch {
      return;
    }
    // Notifications (server/welcome, manifest changes) carry no id.
    if (msg.id === undefined || msg.id === null || !msg.method) return;
    try {
      const id = msg.id as string | number;
      switch (msg.method) {
        case "tool/describe":
          this.replyResult(id, this.describe(msg.params));
          break;
        case "tool/call":
          void this.call(msg.params)
            .then(r => this.replyResult(id, r))
            .catch(e => this.replyError(id, -32603, String(e)));
          break;
        case "provider/getManifest":
          this.reply(id, MANIFEST);
          break;
        default:
          this.replyError(msg.id, -32601, "unknown method " + msg.method);
      }
    } catch (e) {
      this.replyError(msg.id, -32603, String(e));
    }
  }

  private reply(id: string | number, result: unknown) {
    this.send({ jsonrpc: "2.0", id, result });
  }

  private replyError(id: string | number, code: number, message: string) {
    this.send({ jsonrpc: "2.0", id, error: { code, message } });
  }

  // replyResult distinguishes a value from a protocol-level error, so
  // the three failure styles (unknown tool, call-level failure,
  // transport error) each keep their own channel instead of blurring
  // into results.
  private replyResult(id: string | number, r: { error?: string; value?: unknown }) {
    if (r.error !== undefined) this.replyError(id, -32602, r.error);
    else this.reply(id, r.value);
  }

  private describe(params: Record<string, unknown> | undefined): { error?: string; value?: unknown } {
    const name = String(params?.name || "");
    const tool = TOOLS.find(t => t.name === name);
    if (!tool) return { error: "unknown tool " + name };
    return {
      value: {
        name: tool.name,
        title: tool.name,
        description: tool.summary,
        group: tool.group,
        read_only: name !== "page_send" && name !== "page_select_session",
        destructive: false,
        input_schema: INPUT_SCHEMAS[name],
        output_schema: { type: "object" },
      },
    };
  }

  private async call(params: Record<string, unknown> | undefined): Promise<{ error?: string; value?: unknown }> {
    const name = String(params?.name || "");
    const args = (params?.arguments || {}) as Record<string, unknown>;
    const cmds = this.commands;
    if (!cmds) return { error: "page not ready" };
    switch (name) {
      case "page_state":
        return { value: { status: "success", content: cmds.state() } };
      case "page_sessions":
        return { value: { status: "success", content: cmds.sessions() } };
      case "page_select_session": {
        const id = String(args.session_id || "");
        if (!id) return { value: { status: "error", error: "session_id required" } };
        cmds.selectSession(id);
        return { value: { status: "success", content: { selected: id } } };
      }
      case "page_read_conversation":
        return { value: { status: "success", content: cmds.readConversation() } };
      case "page_send": {
        const text = String(args.text ?? "");
        if (!text) return { value: { status: "error", error: "text required" } };
        if (!this.sendEnabled) {
          return {
            value: {
              status: "error",
              error: "page_send is disabled on this page — enable with sai.pageProvider(url, {send: true}) in the browser console",
            },
          };
        }
        const out = await cmds.send(text);
        return out.ok
          ? { value: { status: "success", content: { sent: true, confirmed: !!out.confirmed } } }
          : { value: { status: "error", error: out.error } };
      }
      case "page_text":
        return { value: { status: "success", content: cmds.text() } };
      default:
        return { value: { status: "error", error: "unknown tool " + name } };
    }
  }
}

// ---- enabling ----

const STORAGE_KEY = "sai.pageProvider";

// allowedBrokerHost: only loopback or private-network brokers. The
// debug surface is for the LAN broker that fronts the agent's
// providers; an arbitrary internet wss endpoint must be a deliberate,
// code-edited decision, not a URL away.
export function allowedBrokerHost(url: string): boolean {
  let parsed: URL;
  try {
    parsed = new URL(url);
  } catch {
    return false;
  }
  if (parsed.protocol !== "ws:" && parsed.protocol !== "wss:") return false;
  const host = parsed.hostname.replace(/^\[|\]$/g, "");
  if (host === "localhost" || host === "::1") return true;
  if (/^127\./.test(host)) return true;
  if (/^10\./.test(host)) return true;
  if (/^192\.168\./.test(host)) return true;
  const m172 = /^172\.(\d+)\./.exec(host);
  if (m172) {
    const n = Number(m172[1]);
    if (n >= 16 && n <= 31) return true;
  }
  // IPv6 unique-local (fc00::/7).
  if (/^f[cd]/i.test(host)) return true;
  return false;
}

// pendingPageProviderURL reads the ?page-provider= query parameter and
// strips it from the URL. It does NOT persist and does NOT connect:
// the App turns it into a confirmation banner (session-only connect).
export function pendingPageProviderURL(): string {
  const param = new URLSearchParams(location.search).get("page-provider");
  if (!param) return "";
  try {
    const clean = new URL(location.href);
    clean.searchParams.delete("page-provider");
    history.replaceState(null, "", clean);
  } catch {
    /* private mode */
  }
  return param;
}

// readStoredPageProviderConfig is the auto-connect path: only the
// explicit console action persists (window.sai.pageProvider).
export function readStoredPageProviderConfig(): string {
  try {
    return localStorage.getItem(STORAGE_KEY) || "";
  } catch {
    return "";
  }
}

export function storePageProviderConfig(url: string) {
  try {
    localStorage.setItem(STORAGE_KEY, url);
  } catch {
    /* private mode */
  }
}

export function clearPageProviderConfig() {
  try {
    localStorage.removeItem(STORAGE_KEY);
  } catch {
    /* private mode */
  }
}
