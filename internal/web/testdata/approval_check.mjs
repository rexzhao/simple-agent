// The approval-card fixture (C6c, §16.3): drives the REAL chat
// SessionStore with the approval wire shapes — the snapshot restore
// (a reconnect must not lose a pending decision), the event sequence
// (pending → resolved with all three outcomes), and the tool
// lifecycle clearing — plus the pure decisions (error-code → action
// mapping, double-submit protection, arg truncation). Run by
// TestApprovalFixture via `node approval_check.mjs`.
//
// Scenario blocks push their own names into `done` and the summary
// derives from what actually ran (the K4b/K4d discipline).
import { SessionStore } from "../../../clients/chat/src/store.ts";
import {
  approvalErrorAction,
  approvalButtonsDisabled,
  truncateArgs,
} from "../../../clients/chat/src/newsession.ts";

const failures = [];
const done = [];
function assert(cond, what) {
  if (!cond) failures.push(what);
}

const store = new SessionStore();
store.snap = { ...store.snap, session: "appr0" };

// apply() is TS-private; from plain JS it is the seam this check
// drives (store_check's pattern).
const apply = (frame) => store.apply(frame);

// s10: the pending card from the event sequence.
{
  apply({ kind: "event", t: "tool_approval_pending", item: "i1", name: "shell", args: "rm -rf /tmp/x" });
  const s10card = store.snap.approval;
  assert(s10card && s10card.item === "i1", "s10: the card carries the item identity");
  assert(s10card && s10card.name === "shell", "s10: the card carries the tool name");
  assert(s10card && s10card.args === "rm -rf /tmp/x", "s10: the card carries the arguments");
  assert(s10card && s10card.outcome === null, "s10: a pending card has no outcome");
  done.push("s10");
}

// s11: resolved — all three outcomes land on the card.
{
  apply({ kind: "event", t: "tool_approval_resolved", item: "i1", outcome: "approved" });
  const s11approved = store.snap.approval?.outcome;
  assert(s11approved === "approved", "s11: approved lands on the card");
  // A pending for a DIFFERENT item replaces the card wholesale.
  apply({ kind: "event", t: "tool_approval_pending", item: "i2", name: "web", args: "{}" });
  apply({ kind: "event", t: "tool_approval_resolved", item: "i2", outcome: "denied" });
  const s11denied = store.snap.approval;
  assert(s11denied?.item === "i2" && s11denied?.outcome === "denied", "s11: denied lands on the new card");
  apply({ kind: "event", t: "tool_approval_resolved", item: "i2", outcome: "cancelled" });
  assert(store.snap.approval?.outcome === "cancelled", "s11: cancelled overwrites the outcome");
  // An unmatched resolution is a no-op, not an error.
  apply({ kind: "event", t: "tool_approval_resolved", item: "iZ", outcome: "approved" });
  assert(store.snap.approval?.item === "i2", "s11: an unmatched resolution does not touch the card");
  done.push("s11");
}

// s12: the tool lifecycle clears the card.
{
  // The card's tool completes (tool_result for the slot's call id).
  apply({ kind: "event", t: "tool_started", call_id: "c1", name: "web", args: "{}" });
  apply({ kind: "event", t: "tool_result", call_id: "c1", text: "ok" });
  assert(store.snap.approval === null, "s12: the completed tool clears the card");
  // A settled run clears both the slot and the card.
  apply({ kind: "event", t: "tool_approval_pending", item: "i3", name: "shell", args: "x" });
  apply({ kind: "status", running: false, d: store.snap.head });
  assert(store.snap.approval === null, "s12: a settled run clears the card");
  done.push("s12");
}

// s13: the snapshot restores a PENDING card (the reconnect must not
// lose the decision), and does not resurrect a resolved display.
{
  apply({
    kind: "snapshot",
    anchor: store.snap.head,
    resumed: true,
    records: [],
    running: true,
    tool: { item: "i9", call_id: "c9", name: "shell", args: "long-running", awaiting_approval: true },
    d: store.snap.head,
  });
  const s13restored = store.snap.approval;
  assert(
    s13restored && s13restored.item === "i9" && s13restored.outcome === null,
    "s13: the snapshot restores the pending card",
  );
  // Not awaiting: the tool slot alone, no card.
  apply({
    kind: "snapshot",
    anchor: store.snap.head,
    resumed: true,
    records: [],
    running: true,
    tool: { item: "i10", call_id: "c10", name: "shell", args: "plain", awaiting_approval: false },
    d: store.snap.head,
  });
  assert(store.snap.approval === null, "s13: a plain tool slot restores without a card");
  done.push("s13");
}

// s14: the error-code → action mapping (session-level codes first).
{
  const s14unknown = approvalErrorAction({ status: 404, code: "approval_unknown" });
  const s14resolved = approvalErrorAction({ status: 409, code: "approval_resolved" });
  const s14session = approvalErrorAction({ status: 404, code: "unknown_session" });
  const s14closing = approvalErrorAction({ status: 503, code: "session_closing" });
  const s14server = approvalErrorAction({ status: 500, code: "" });
  assert(s14unknown.kind === "collapse", "s14: approval_unknown collapses the card");
  assert(s14resolved.kind === "wait", "s14: approval_resolved waits for the event (no resubmit)");
  assert(s14session.kind === "collapse", "s14: the session-level 404 wins over the approval reading");
  assert(s14closing.kind === "hint", "s14: session_closing hints a retry");
  assert(s14server.kind === "hint" && s14server.hint.includes("try again"), "s14: 5xx hints a retry");
  done.push("s14");
}

// s15: double-submit protection — the buttons lock while submitting
// and once resolved; the args preview truncates on a rune boundary.
{
  const s15idle = approvalButtonsDisabled(false, null);
  const s15busy = approvalButtonsDisabled(true, null);
  const s15done = approvalButtonsDisabled(false, "approved");
  assert(s15idle === false, "s15: idle pending card has live buttons");
  assert(s15busy === true, "s15: submitting locks both buttons");
  assert(s15done === true, "s15: a resolved card keeps the buttons locked");
  const s15short = truncateArgs("ls -la");
  const s15long = truncateArgs("x".repeat(300), 160);
  const s15cjk = truncateArgs("中".repeat(200), 160);
  assert(s15short === "ls -la", "s15: short args pass through");
  assert(s15long.length === 161 && s15long.endsWith("…"), "s15: long args truncate with the ellipsis");
  assert(!s15cjk.includes("…") || [...s15cjk.slice(0, -1)].every(c => c === "中"), "s15: CJK args truncate on rune boundaries");
  assert([...truncateArgs("中".repeat(200), 160)].length === 161, "s15: the CJK cut is 160 runes + ellipsis");
  done.push("s15");
}

if (failures.length) {
  console.error(failures.map((f) => "FAIL " + f).join("\n"));
  process.exit(1);
}
console.log(`approval_check: OK (${done.length} scenarios: ${done.join(",")})`);
