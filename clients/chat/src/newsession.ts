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

// deleteRetryable: the closing-window and still-open refusals clear
// on their own (detach + sweep); the delete flow retries these
// instead of surfacing them.
export function deleteRetryable(code: string): boolean {
  return code === "session_open" || code === "session_closing";
}
