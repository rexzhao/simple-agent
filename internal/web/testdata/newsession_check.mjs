// New-session dialog logic check (C6a): the pure decisions behind the
// React components — /new exact-match interception, default
// selection, reasoning-tier visibility, and the error-code → hint
// mappings for create and delete. Run by TestNewSessionFixture via
// `node newsession_check.mjs` (type stripping, like store_check).
//
// Scenario-block integrity (the B2-2b K4b/K4d lesson): every block
// declares a BLOCK-SCOPED marker const and pushes its own name into
// `done` — deleting a block removes the marker from `done` and the
// summary line changes; a block deleted with its push line but
// without its asserts still fails its marker reference. The runner
// (TestNewSessionFixture) asserts the EXACT expected list, so a
// missing or renamed scenario cannot pass silently.
import {
  isSlashNew,
  defaultModel,
  reasoningLevelsOf,
  createSessionHint,
  deleteBlockedHint,
  deleteRetryable,
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
  const s5retryOpen = deleteRetryable("session_open");
  const s5retryClosing = deleteRetryable("session_closing");
  const s5retryRun = deleteRetryable("run_active");
  const s5retryUnknown = deleteRetryable("");
  assert(s5run?.includes("stop it"), "s5: run_active tells the user to stop the run");
  assert(s5open === null, "s5: session_open is not a user block");
  assert(s5retryOpen === true, "s5: session_open retries");
  assert(s5retryClosing === true, "s5: session_closing retries");
  assert(s5retryRun === false, "s5: run_active does NOT retry");
  assert(s5retryUnknown === false, "s5: unknown codes do not retry");
  done.push("s5");
}

if (failures.length) {
  console.error(failures.map((f) => "FAIL " + f).join("\n"));
  process.exit(1);
}
console.log(`newsession_check: OK (${done.length} scenarios: ${done.join(",")})`);
