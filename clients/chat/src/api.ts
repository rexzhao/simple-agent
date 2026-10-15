// The serve API client. Same-origin when mounted at /chat
// (-http-static); in dev the Vite proxy carries /api to the running
// serve, and the serve side answers CORS preflights for loopback
// origins.
import type { HistoryResponse, ProjectsResponse, StateResponse } from "./types";
import type { ModelDescriptor } from "./newsession";

export class APIError extends Error {
  status: number;
  code: string;

  constructor(status: number, code: string, message: string) {
    super(message || code || `HTTP ${status}`);
    this.status = status;
    this.code = code;
  }
}

// postJSON/putJSON/deleteJSON read the body as text ONCE and parse
// locally: a response body is single-use, and the error bodies are
// plain text outside the JSON contract.
async function request<T>(url: string, method: string, body?: unknown): Promise<T> {
  const res = await fetch(url, {
    method,
    headers: body !== undefined ? { "Content-Type": "application/json" } : undefined,
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  const raw = await res.text().catch(() => "");
  if (!res.ok) {
    let code = "";
    try {
      code = (JSON.parse(raw) || {}).error || "";
    } catch {
      /* plain-text error body */
    }
    throw new APIError(res.status, code, raw);
  }
  if (raw === "") return undefined as T;
  try {
    return JSON.parse(raw) as T;
  } catch {
    // A 200 with an unparseable body is a broken contract, not a
    // success — surface it here instead of a property access later.
    throw new APIError(res.status, "", "bad JSON in 200 response");
  }
}

export const getProjects = () => request<ProjectsResponse>("/api/projects", "GET");
export const getState = (id: string) => request<StateResponse>(`/api/s/${id}/state`, "GET");
export const getHistory = (id: string, before: string) =>
  request<HistoryResponse>(`/api/s/${id}/history?before=${encodeURIComponent(before)}&limit=50`, "GET");
export const postSend = (id: string, text: string) =>
  request<unknown>(`/api/s/${id}/send`, "POST", { text });
export const postSteer = (id: string, text: string) =>
  request<unknown>(`/api/s/${id}/steer`, "POST", { text });
export const putSteer = (id: string, item: string, text: string) =>
  // The update endpoint reads {item, text} from the BODY (only the
  // withdraw takes the item as a query parameter).
  request<unknown>(`/api/s/${id}/steer`, "PUT", { item, text });
export const deleteSteer = (id: string, item: string) =>
  request<unknown>(`/api/s/${id}/steer?item=${encodeURIComponent(item)}`, "DELETE");
export const postStop = (id: string) => request<unknown>(`/api/s/${id}/stop`, "POST", {});
export const deleteSession = (id: string) => request<unknown>(`/api/s/${id}`, "DELETE");
export const getProjectsAll = () =>
  request<ProjectsResponse>("/api/projects?include_archived=1", "GET");

export const archiveSession = (id: string) =>
  request<unknown>(`/api/s/${id}/archive`, "POST", {});

export const postApproval = (
  id: string,
  item: string,
  allow: boolean,
  reason?: string,
) =>
  request<unknown>(`/api/s/${id}/approval`, "POST", {
    item,
    allow,
    reason: reason ?? "",
  });

export const unarchiveSession = (id: string) =>
  request<unknown>(`/api/s/${id}/archive`, "DELETE");

export const getModels = () =>
  request<ModelDescriptor[]>("/api/models", "GET");

// createSession posts the new-session form (§17.2): the body carries
// only the meta fields, reasoning_level ONLY when set ("" means "the
// model's default" and the server treats absence identically — but
// the strict body makes the explicit-empty form a future
// compatibility risk for no gain).
export const createSession = (
  pid: string,
  meta: { provider: string; model: string; reasoning_level?: string },
) => {
  const body: Record<string, string> = {
    provider: meta.provider,
    model: meta.model,
  };
  if (meta.reasoning_level) body.reasoning_level = meta.reasoning_level;
  return request<{ id: string; ns: string }>(
    `/api/projects/${encodeURIComponent(pid)}/sessions`,
    "POST",
    body,
  );
};
