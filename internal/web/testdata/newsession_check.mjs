// New-session dialog logic check (C6a): the pure decisions behind the
// React components — /new exact-match interception, default
// selection, reasoning-tier visibility, and the error-code → hint
// mappings for create and delete. Run by TestNewSessionFixture via
// `node newsession_check.mjs` (type stripping, like store_check).
//
// Scenario-block integrity (the B2-2b K4b/K4d lesson): every block
// pushes its own name into `done` and the summary derives from the
// blocks that actually ran — deleting a block (or its push line)
// changes the summary list. The runner (TestNewSessionFixture)
// asserts the EXACT expected list, so a missing or renamed scenario
// cannot pass silently.
import {
  isSlashNew,
  defaultModel,
  reasoningLevelsOf,
  createSessionHint,
  deleteBlockedHint,
  refusalRetryable,
  archiveBlockedHint,
  partitionSessions,
  opStep,
  stateProbeHint,
} from "../../../clients/chat/src/newsession.ts";

const failures = [];
const done = [];
function assert(cond, what) {
  if (!cond) failures.push(what);
}

// s1: /new interception is EXACT — /newfoo and /new with args are
// ordinary messages.
{
  const s1exact = isSlashNew("/new");
  const s1foo = isSlashNew("/newfoo");
  const s1args = isSlashNew("/new -provider x");
  const s1ws = isSlashNew(" /new ");
  const s1plain = isSlashNew("hello");
  const s1empty = isSlashNew("");
  assert(s1exact === true, "s1: /new must open the dialog");
  assert(s1foo === false, "s1: /newfoo must send as a message");
  assert(s1args === false, "s1: /new with args must send");
  assert(s1ws === true, "s1: surrounding whitespace still /new");
  assert(s1plain === false, "s1: plain text is not /new");
  assert(s1empty === false, "s1: empty is not /new");
  done.push("s1");
}

// s2: default selection — the marked default, else the first entry.
{
  const models = [
    { provider: "a", model: "a1", reasoning_levels: null, default: false },
    { provider: "a", model: "a2", reasoning_levels: ["low", "high"], default: true },
    { provider: "b", model: "b1", reasoning_levels: [], default: false },
  ];
  const s2marked = defaultModel(models)?.model;
  const s2first = defaultModel([models[0], models[2]])?.model;
  const s2none = defaultModel([]);
  assert(s2marked === "a2", "s2: the marked default is preselected");
  assert(s2first === "a1", "s2: no default mark → first entry");
  assert(s2none === null, "s2: no models → null (the dialog explains)");
  done.push("s2");
}

// s3: reasoning visibility — non-empty tiers shown, null/[]/absent
// hidden; empty-string tiers filtered out.
{
  const models = [
    { provider: "a", model: "a1", reasoning_levels: null, default: false },
    { provider: "a", model: "a2", reasoning_levels: ["low", "high"], default: true },
    { provider: "b", model: "b1", reasoning_levels: [], default: false },
  ];
  const s3tiers = JSON.stringify(reasoningLevelsOf(models, "a", "a2"));
  const s3null = reasoningLevelsOf(models, "a", "a1").length;
  const s3empty = reasoningLevelsOf(models, "b", "b1").length;
  const s3unknown = reasoningLevelsOf(models, "z", "z9").length;
  const s3blank = reasoningLevelsOf(
    [{ provider: "a", model: "a1", reasoning_levels: ["", "low"], default: false }],
    "a",
    "a1",
  ).length;
  assert(s3tiers === JSON.stringify(["low", "high"]), "s3: tiers of the configured model");
  assert(s3null === 0, "s3: null levels → hidden");
  assert(s3empty === 0, "s3: empty levels → hidden");
  assert(s3unknown === 0, "s3: unknown model → hidden");
  assert(s3blank === 1, "s3: blank tiers filtered");
  done.push("s3");
}

// s4: create-error hints — every code the endpoint can answer maps to
// a readable line; unknown codes and 5xx fall back.
{
  const s4unexpected = createSessionHint({ status: 400, code: "unexpected_field" });
  const s4invalid = createSessionHint({ status: 400, code: "invalid_input" });
  const s4project = createSessionHint({ status: 404, code: "unknown_project" });
  const s4model = createSessionHint({ status: 409, code: "model_unavailable" });
  const s4server = createSessionHint({ status: 500, code: "" });
  const s4teapot = createSessionHint({ status: 418, code: "teapot" });
  assert(s4unexpected.includes("reload"), "s4: unexpected_field hints a reload");
  assert(s4invalid.includes("required"), "s4: invalid_input names the fields");
  assert(s4project.includes("registered"), "s4: unknown_project says unregistered");
  assert(s4model.includes("pick another"), "s4: model_unavailable says pick another");
  assert(s4server.includes("try again"), "s4: 5xx falls back to retry wording");
  assert(s4teapot.length > 0, "s4: unknown code still yields a line");
  done.push("s4");
}

// s5: delete refusals — run_active is user-actionable, the closing
// window codes retry silently, others surface raw.
{
  const s5run = deleteBlockedHint({ status: 409, code: "run_active" });
  const s5open = deleteBlockedHint({ status: 409, code: "session_open" });
  const s5retryOpen = refusalRetryable("session_open");
  const s5retryClosing = refusalRetryable("session_closing");
  const s5retryRun = refusalRetryable("run_active");
  const s5retryUnknown = refusalRetryable("");
  assert(s5run?.includes("stop it"), "s5: run_active tells the user to stop the run");
  assert(s5open === null, "s5: session_open is not a user block");
  assert(s5retryOpen === true, "s5: session_open retries");
  assert(s5retryClosing === true, "s5: session_closing retries");
  assert(s5retryRun === false, "s5: run_active does NOT retry");
  assert(s5retryUnknown === false, "s5: unknown codes do not retry");
  done.push("s5");
}

// s6: sidebar partition — flagged rows go to the archived section,
// the working set keeps the rest, order preserved.
{
  const rows = [
    { id: "a", archived: false },
    { id: "b", archived: true },
    { id: "c" },
    { id: "d", archived: true },
  ];
  const s6split = partitionSessions(rows);
  assert(
    JSON.stringify(s6split.live.map((r) => r.id)) === JSON.stringify(["a", "c"]),
    "s6: live section keeps unflagged rows in order",
  );
  assert(
    JSON.stringify(s6split.archived.map((r) => r.id)) === JSON.stringify(["b", "d"]),
    "s6: archived section collects flagged rows in order",
  );
  assert(partitionSessions([]).live.length === 0, "s6: empty listing → empty sections");
  done.push("s6");
}

// s7: archive/restore refusals — run_active and session_locked are
// user-actionable; the closing-window codes retry silently.
{
  const s7run = archiveBlockedHint({ status: 409, code: "run_active" });
  const s7locked = archiveBlockedHint({ status: 409, code: "session_locked" });
  const s7open = archiveBlockedHint({ status: 409, code: "session_open" });
  assert(s7run?.includes("stop it"), "s7: run_active tells the user to stop the run");
  assert(s7locked?.includes("another process"), "s7: session_locked names the other holder");
  assert(s7open === null, "s7: session_open is not a user block (retries)");
  done.push("s7");
}

// s8: the shared retry step — done on success, retry while under the
// attempt budget, hint at the boundary (the flow TERMINATES) and for
// non-retryable codes.
{
  const s8done = opStep(0, 10, null, deleteBlockedHint, "fallback");
  const s8retry = opStep(3, 10, { status: 409, code: "session_open" }, deleteBlockedHint, "fallback");
  const s8boundary = opStep(9, 10, { status: 409, code: "session_open" }, deleteBlockedHint, "fallback");
  const s8run = opStep(0, 10, { status: 409, code: "run_active" }, deleteBlockedHint, "fallback");
  const s8unknown = opStep(0, 10, { status: 500, code: "" }, deleteBlockedHint, "fallback");
  assert(s8done.kind === "done", "s8: success is done");
  assert(s8retry.kind === "retry", "s8: retryable code under budget retries");
  assert(
    s8boundary.kind === "hint" && s8boundary.hint === "fallback",
    "s8: at the attempt budget the flow terminates with the fallback hint",
  );
  assert(s8run.kind === "hint" && s8run.hint.includes("stop it"), "s8: run_active hints immediately");
  assert(s8unknown.kind === "hint" && s8unknown.hint === "fallback", "s8: unknown errors surface the fallback");
  done.push("s8");
}

// s9: the deep-link probe — 404 bounces with the hint;
// locked/archived present and stay; other statuses are null.
{
  const s9missing = stateProbeHint({ status: 404, code: "unknown_session" });
  const s9locked = stateProbeHint({ status: 409, code: "session_locked" });
  const s9archived = stateProbeHint({ status: 409, code: "session_archived" });
  const s9server = stateProbeHint({ status: 500, code: "" });
  assert(s9missing?.includes("not found"), "s9: 404 bounces with the not-found hint");
  assert(s9locked?.includes("locked"), "s9: session_locked presents the conflict");
  assert(
    s9archived?.includes("archived") && s9archived.includes("restore"),
    "s9: an archived deep link hints the restore path (NOT silently swallowed)",
  );
  assert(s9server === null, "s9: 5xx is not a route hint");
  done.push("s9");
}

if (failures.length) {
  console.error(failures.map((f) => "FAIL " + f).join("\n"));
  process.exit(1);
}
console.log(`newsession_check: OK (${done.length} scenarios: ${done.join(",")})`);
