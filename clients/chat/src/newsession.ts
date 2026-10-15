// New-session and delete-confirm dialog logic (C6a, §17.6): the pure
// state decisions, kept out of the React components so the node
// fixture (internal/web/testdata/newsession_check.mjs) can drive them
// directly — defaults, reasoning visibility, and the error-code →
// user-hint mappings are the parts worth pinning.

// ModelDescriptor mirrors GET /api/models' wire shape (§17.2). The
// server marshals an absent reasoning config as null; an explicitly
// empty tier list also arrives as [] — both mean "no reasoning
// configuration".
export type ModelDescriptor = {
  provider: string;
  model: string;
  reasoning_levels: string[] | null;
  default: boolean;
};

// isSlashNew is the composer's /new interception: EXACT match only.
// "/newfoo" is an ordinary message (the CLI's exact-match contract,
// carried into the chat client); "/new -provider x" is too — the
// dialog is the chat client's only creation surface.
export function isSlashNew(text: string): boolean {
  return text.trim() === "/new";
}

// defaultModel picks the dialog's preselected entry: the marked
// default, else the first listed, else null (no models — the dialog
// shows that instead of an empty selector).
export function defaultModel(models: ModelDescriptor[]): ModelDescriptor | null {
  return models.find((m) => m.default) ?? models[0] ?? null;
}

// reasoningLevelsOf returns the selected model's reasoning tiers;
// empty means the model takes no reasoning configuration and the
// dialog hides the selector.
export function reasoningLevelsOf(
  models: ModelDescriptor[],
  provider: string,
  model: string,
): string[] {
  const found = models.find((m) => m.provider === provider && m.model === model);
  return (found?.reasoning_levels ?? []).filter((lvl) => lvl !== "");
}

// CreateSessionError is the shape the hint mapper needs — APIError
// satisfies it structurally.
export type CodeError = { status: number; code: string };

// createSessionHint maps a failed POST /api/projects/{pid}/sessions
// onto a user-facing line. The dialog stays open with this hint; the
// user can fix the selection and retry.
export function createSessionHint(e: CodeError): string {
  switch (e.code) {
    case "unexpected_field":
      return "unexpected field — this page is older than the server; reload it";
    case "invalid_input":
      return "provider and model are required";
    case "unknown_project":
      return "that project is no longer registered — pick another";
    case "model_unavailable":
      return "the selected model is no longer in the configuration — pick another";
    default:
      if (e.status >= 500) return "server error — try again";
      return "could not create the session";
  }
}

// deleteBlockedHint maps a delete refusal the USER must act on.
// null means "not a user-actionable block" — the retryable codes
// (deleteRetryable) loop silently, anything else surfaces raw.
export function deleteBlockedHint(e: CodeError): string | null {
  if (e.code === "run_active")
    return "the session is running — stop it before deleting";
  if (e.code === "model_unavailable")
    return "the session's model no longer resolves — delete it from the CLI";
  return null;
}

// refusalRetryable: the closing-window and still-open refusals clear
// on their own (detach + sweep); the delete and archive flows retry
// these instead of surfacing them.
export function refusalRetryable(code: string): boolean {
  return code === "session_open" || code === "session_closing";
}

// ---- C6b: the archive sidebar section ----

// archiveBlockedHint maps an archive/restore refusal the USER must
// act on; null means "not user-actionable" (the retryable codes loop
// silently, anything else surfaces raw).
export function archiveBlockedHint(e: CodeError): string | null {
  if (e.code === "run_active")
    return "the session is running — stop it before archiving";
  if (e.code === "session_locked")
    return "another process holds the session — close it first";
  return null;
}

// SidebarRow is one /api/projects?include_archived=1 session row.
export type SidebarRow = { id: string; archived?: boolean };

// partitionSessions splits the listing into the working set and the
// parked rows — the sidebar's two sections (§17.3: archived hidden
// from the working set, shown collapsed at the bottom).
export function partitionSessions<T extends SidebarRow>(rows: T[]): {
  live: T[];
  archived: T[];
} {
  const live: T[] = [];
  const archived: T[] = [];
  for (const row of rows) (row.archived ? archived : live).push(row);
  return { live, archived };
}

// ---- the shared bounded-retry step (delete C6a, archive C6b) ----

// OpStep is one decision of a delete-or-archive flow: the request
// succeeded (done), a self-healing refusal says try again (retry —
// the caller sleeps and re-sends), or the user must act (hint).
export type OpStep =
  | { kind: "done" }
  | { kind: "retry" }
  | { kind: "hint"; hint: string };

// opStep decides the next move after one attempt: e null means the
// request succeeded; retryable codes retry until maxAttempts (then
// the fallback hint — the flow must TERMINATE, never loop forever);
// everything else maps through blockedHint (null → the fallback).
// The BUSY flag spans the whole flow by construction: the caller
// stays disabled from the first attempt until a done or a hint, so a
// second click cannot start a concurrent loop mid-retry.
export function opStep(
  attempt: number,
  maxAttempts: number,
  e: CodeError | null,
  blockedHint: (e: CodeError) => string | null,
  retryExhaustedHint: string,
  fallback: string,
): OpStep {
  if (!e) return { kind: "done" };
  if (refusalRetryable(e.code)) {
    // Under the budget: the closing window may self-heal. AT the
    // budget: the code did NOT clear — someone else holds the
    // session open, and the user needs to know that (not a generic
    // "try again").
    if (attempt + 1 < maxAttempts) return { kind: "retry" };
    return { kind: "hint", hint: retryExhaustedHint };
  }
  return { kind: "hint", hint: blockedHint(e) ?? fallback };
}

// ---- the deep-link probe (the log view's unknown-id handling) ----

// stateProbeHint maps a failed GET /api/s/{id}/state probe — the
// pre-connect existence check an SSE stream cannot make — onto the
// route hint. 404 bounces to the list (the session was deleted or
// never existed); 409 presents the conflict and stays.
export function stateProbeHint(e: CodeError): string | null {
  if (e.status === 404) return "session not found — deleted or unknown; back to the list";
  if (e.code === "session_locked")
    return "session is locked by another process";
  if (e.code === "session_archived")
    return "this session is archived — restore it from the sidebar";
  return null;
}

// ---- C6c: the approval card ----

// ApprovalErrorAction is what a failed POST /approval means for the
// card: collapse it (the approval is gone — nothing to answer),
// hold it and wait (someone else answered; the resolved EVENT is
// coming — resubmitting would just race), or surface a hint.
export type ApprovalErrorAction =
  | { kind: "collapse"; hint: string }
  | { kind: "wait"; hint: string }
  | { kind: "hint"; hint: string };

export function approvalErrorAction(e: CodeError): ApprovalErrorAction {
  // Session-level codes first (§16.3): the approval route resolves
  // the session before the approval itself.
  if (e.status === 404 && e.code === "unknown_session")
    return { kind: "collapse", hint: "the session is gone" };
  if (e.status === 409 && e.code === "session_closing")
    return { kind: "hint", hint: "the session is closing — retry in a moment" };
  if (e.code === "approval_unknown")
    return { kind: "collapse", hint: "the approval was already handled — closing" };
  if (e.code === "approval_resolved")
    return { kind: "wait", hint: "answered elsewhere — waiting for the resolution" };
  if (e.status >= 500) return { kind: "hint", hint: "server error — try again" };
  return { kind: "hint", hint: "could not submit the decision — try again" };
}

// approvalButtonsDisabled: both buttons lock while a decision is in
// flight (one POST decides; a second would race the first) and stay
// locked once resolved (the card is an outcome display by then).
export function approvalButtonsDisabled(submitting: boolean, outcome: string | null): boolean {
  return submitting || outcome !== null;
}

// truncateArgs renders the card's argument preview: one line, cut on
// a rune boundary, expandable in the UI.
export function truncateArgs(args: string, max = 160): string {
  if ([...args].length <= max) return args;
  return [...args].slice(0, max).join("") + "…";
}
