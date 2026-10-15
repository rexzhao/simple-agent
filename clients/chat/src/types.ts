// Wire types mirrored from the serve side (internal/web/wire.go,
// storage/record.go, internal/web/manager.go). The chat client
// consumes the same contract as the log page — these are its shapes.

export type WireToolCall = {
  id: string;
  name: string;
  arguments: string;
};

export type Record = {
  item?: string;
  t: string;
  ts: string;
  run?: string;
  iter?: number;
  provider?: string;
  model?: string;
  cwd?: string;
  content?: string;
  reasoning?: string;
  tool_calls?: WireToolCall[];
  tool_call_id?: string;
  tool_name?: string;
  is_error?: boolean;
  error?: string;
};

export type StreamingWire = { item: string; content?: string; reasoning?: string };
export type ToolWire = { item: string; call_id?: string; name?: string; args?: string; awaiting_approval?: boolean };
export type SteeredWire = { item: string; text: string };

export type Frame = {
  kind: string;
  anchor?: string;
  resumed?: boolean;
  total?: number;
  more?: boolean;
  records?: Record[];
  running?: boolean;
  streaming?: StreamingWire;
  tool?: ToolWire;
  steered?: SteeredWire[];
  r?: Record;
  t?: string;
  item?: string;
  text?: string;
  call_id?: string;
  name?: string;
  args?: string;
  is_err?: boolean;
  outcome?: string; // tool_approval_resolved: approved / denied / cancelled
  d?: string;
  n?: number;
  compacting?: boolean;
  notice?: string;
};

export type StateResponse = {
  session: string;
  running: boolean;
  compacting: boolean;
  count: number;
  digest: string;
  steered?: SteeredWire[];
};

export type ProjectsResponse = {
  projects: {
    id: string;
    path: string;
    sessions: {
      id: string;
      title: string;
      created: string;
      active: boolean;
      archived?: boolean;
    }[];
  }[];
  registry_ok: boolean;
};

export type HistoryResponse = {
  records: Record[];
  anchor: string;
  more: boolean;
};
