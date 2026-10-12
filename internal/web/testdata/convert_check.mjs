// The convert check: fixture records and live items flow through the
// chat client's conversion (convertRecords/convertLive →
// toThreadMessageLike) into the REAL runtime validator
// (fromThreadMessageLike) — any throw is red. This covers the entire
// validator category, including rules the library adds later, instead
// of restating invariants we believe (the status-on-user crash was
// exactly that class: our shape, their validator, no check in
// between).
//
// The validator is imported from clients/chat/node_modules by file
// URL (package exports block the deep specifier); if node_modules is
// absent (no npm install), the check skips.
import { convertRecords, convertLive, toThreadMessageLike } from "../../../clients/chat/src/convert.ts";

const failures = [];
function assert(cond, what) {
  if (!cond) failures.push(what);
}

let validator;
try {
  const url = new URL(
    "../../../clients/chat/node_modules/@assistant-ui/core/dist/runtime/utils/thread-message-like.js",
    import.meta.url,
  );
  validator = (await import(url.href)).fromThreadMessageLike;
} catch {
  console.log("convert_check: SKIP (no node_modules — run npm install in clients/chat)");
  process.exit(0);
}

const rec = (t, extra) => ({ item: "it-" + t + "-" + Math.random().toString(36).slice(2, 6), t, ts: "2026-10-10T00:00:00Z", ...extra });

// A mixed record stream: header, conversation with reasoning + tool
// calls, JOINed tool results, a run failure, a compaction record (no
// chat rendering), a second run.
const records = [
  rec("session.created", { provider: "p", model: "m", cwd: "F:/x" }),
  rec("user.input", { run: "r1", content: "question" }),
  rec("assistant.message", { run: "r1", content: "answer", reasoning: "thoughts", tool_calls: [{ id: "c1", name: "shell", arguments: "{\"cmd\":\"ls\"}" }] }),
  rec("tool.result", { run: "r1", tool_call_id: "c1", tool_name: "shell", content: "file list", is_error: false }),
  rec("context.compacted", { run: "r1", content: "summary" }),
  rec("user.input", { run: "r2", content: "again" }),
  rec("run.failed", { run: "r2", error: "provider exploded" }),
];

const msgs = convertRecords(records);
assert(msgs.length === 4, "c1 message count (header/compaction dropped): " + msgs.length);
const asst1 = msgs[1];
assert(asst1.role === "assistant" && asst1.parts.length === 3, "c1 assistant parts (reasoning+text+tool)");
const toolPart = asst1.parts[2];
assert(toolPart.type === "tool-call" && toolPart.result === "file list", "c1 tool result JOINed");
const failed = msgs[msgs.length - 1];
assert(failed.status?.type === "incomplete", "c1 run.failed folds to incomplete");

// Every message through the REAL validator.
for (const m of msgs) {
  try {
    const out = validator(toThreadMessageLike(m, 0), undefined);
    assert(out.role === m.role, "c2 role preserved");
  } catch (e) {
    failures.push("c2 validator threw for " + m.role + "/" + m.id + ": " + e.message);
  }
}

// User messages must carry no status key at all (the crash class).
const userLike = toThreadMessageLike(msgs[0], 0);
assert(!("status" in userLike), "c3 user message has no status key");

// A live item: running status, in-flight tool part.
const live = {
  id: "live-1",
  parts: [
    { type: "reasoning", text: "thinking" },
    { type: "text", text: "partial" },
    { type: "tool-call", toolCallId: "c2", toolName: "shell", argsText: "{}", running: true },
  ],
  failed: false,
};
const liveMsg = convertLive(live);
assert(!!liveMsg && liveMsg.status.type === "running", "c4 live running");
try {
  validator(toThreadMessageLike(liveMsg, 0), undefined);
} catch (e) {
  failures.push("c4 live validator threw: " + e.message);
}
const failedLive = convertLive({ ...live, failed: true });
assert(failedLive.status.type === "incomplete", "c4 failed live incomplete");

// Edge shapes: empty text user, empty parts assistant, tool result
// with no parent in the window (dropped), failed run with no same-run
// message (synthetic row).
const edge = [
  rec("user.input", { run: "r3", content: "" }),
  rec("assistant.message", { run: "r3" }),
  rec("tool.result", { run: "r4", tool_call_id: "orphan", tool_name: "x", content: "lost" }),
  rec("run.failed", { run: "r5", error: "e" }),
];
const edgeMsgs = convertRecords(edge);
for (const m of edgeMsgs) {
  try {
    validator(toThreadMessageLike(m, 0), undefined);
  } catch (e) {
    failures.push("c5 edge validator threw: " + m.role + ": " + e.message);
  }
}

if (failures.length) {
  console.error("convert_check: FAILED");
  for (const f of failures) console.error("  - " + f);
  process.exit(1);
}
console.log("convert_check: OK (validator accepted all shapes)");
