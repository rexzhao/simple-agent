// The serve API client. Same-origin when mounted at /chat
// (-http-static); in dev the Vite proxy carries /api to the running
// serve, and the serve side answers CORS preflights for loopback
// origins.
import type { HistoryResponse, ProjectsResponse, StateResponse } from "./types";

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
  try {
    return JSON.parse(raw) as T;
  } catch {
    return undefined as T;
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
  request<unknown>(`/api/s/${id}/steer?item=${encodeURIComponent(item)}`, "PUT", { text });
export const deleteSteer = (id: string, item: string) =>
  request<unknown>(`/api/s/${id}/steer?item=${encodeURIComponent(item)}`, "DELETE");
export const postStop = (id: string) => request<unknown>(`/api/s/${id}/stop`, "POST", {});
export const deleteSession = (id: string) => request<unknown>(`/api/s/${id}`, "DELETE");
