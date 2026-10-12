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
// the old registration when the socket closes).
//
// Enabling: ?page-provider=<ws-url> on the page URL (persisted to
// localStorage), or window.sai.pageProvider(url) from the console.
// Unset, the module is inert — zero behavior change.

export type PageCommands = {
  state(): Record<string, unknown>;
  sessions(): unknown[];
  selectSession(id: string): void;
  readConversation(): unknown[];
  send(text: string): { ok: boolean; error?: string };
  text(): string;
};

type ToolEntry = { name: string; summary: string; group: string };

const TOOLS: ToolEntry[] = [
  { name: "page_state", summary: "The page's current view state: session, connection, run state, chain verification, steer queue, URL.", group: "page" },
  { name: "page_sessions", summary: "The sessions the page lists (the projects poll view).", group: "page" },
  { name: "page_select_session", summary: "Switch the page to a session by id (the real route switch).", group: "page" },
  { name: "page_read_conversation", summary: "The currently rendered conversation (roles and parts) — what the human sees.", group: "page" },
  { name: "page_send", summary: "Type a message into the composer and click send — the real UI path, not the API.", group: "page" },
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
    switch (msg.method) {
      case "tool/describe":
        this.reply(msg.id, this.describe(msg.params));
        break;
      case "tool/call":
        this.reply(msg.id, this.call(msg.params));
        break;
      case "provider/getManifest":
        this.reply(msg.id, MANIFEST);
        break;
      default:
        this.replyError(msg.id, -32601, "unknown method " + msg.method);
    }
  }

  private reply(id: string | number, result: unknown) {
    this.send({ jsonrpc: "2.0", id, result });
  }

  private replyError(id: string | number, code: number, message: string) {
    this.send({ jsonrpc: "2.0", id, error: { code, message } });
  }

  private describe(params: Record<string, unknown> | undefined) {
    const name = String(params?.name || "");
    const tool = TOOLS.find(t => t.name === name);
    if (!tool) return { error: "unknown tool " + name };
    return {
      name: tool.name,
      title: tool.name,
      description: tool.summary,
      group: tool.group,
      read_only: name !== "page_send" && name !== "page_select_session",
      destructive: false,
      input_schema: INPUT_SCHEMAS[name],
      output_schema: { type: "object" },
    };
  }

  private call(params: Record<string, unknown> | undefined) {
    const name = String(params?.name || "");
    const args = (params?.arguments || {}) as Record<string, unknown>;
    const cmds = this.commands;
    if (!cmds) return { status: "error", error: "page not ready" };
    try {
      switch (name) {
        case "page_state":
          return { status: "success", content: cmds.state() };
        case "page_sessions":
          return { status: "success", content: cmds.sessions() };
        case "page_select_session": {
          const id = String(args.session_id || "");
          if (!id) return { status: "error", error: "session_id required" };
          cmds.selectSession(id);
          return { status: "success", content: { selected: id } };
        }
        case "page_read_conversation":
          return { status: "success", content: cmds.readConversation() };
        case "page_send": {
          const text = String(args.text ?? "");
          if (!text) return { status: "error", error: "text required" };
          const out = cmds.send(text);
          return out.ok ? { status: "success", content: { sent: true } } : { status: "error", error: out.error };
        }
        case "page_text":
          return { status: "success", content: cmds.text() };
        default:
          return { status: "error", error: "unknown tool " + name };
      }
    } catch (e) {
      return { status: "error", error: String(e) };
    }
  }
}

// ---- enabling ----

const STORAGE_KEY = "sai.pageProvider";

export function readPageProviderConfig(): string {
  const param = new URLSearchParams(location.search).get("page-provider");
  if (param) {
    try {
      localStorage.setItem(STORAGE_KEY, param);
    } catch {
      /* private mode */
    }
    return param;
  }
  try {
    return localStorage.getItem(STORAGE_KEY) || "";
  } catch {
    return "";
  }
}

export function clearPageProviderConfig() {
  try {
    localStorage.removeItem(STORAGE_KEY);
  } catch {
    /* private mode */
  }
}
