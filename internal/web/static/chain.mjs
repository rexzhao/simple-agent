// The digest chain, shared by the page and the CI cross-implementation
// check (node runs this exact module against Go-generated fixtures).
// It must stay byte-for-byte identical to internal/web/wire.go.

export const chainVersion = "sai/v1/";

export async function sha256Hex(text) {
  const data = new TextEncoder().encode(text);
  const digest = await crypto.subtle.digest("SHA-256", data);
  return [...new Uint8Array(digest)].map(b => b.toString(16).padStart(2, "0")).join("");
}

// chainInput builds the length-prefixed encoding of one record's tuple.
// Lengths are UTF-8 BYTE counts (Go's len), not UTF-16 code units —
// "你好" is 6, not 2. This was a real cross-language bug; the fixture
// test pins it.
export function chainInput(r) {
  const enc = new TextEncoder();
  let s = "";
  const field = v => { s += enc.encode(v).length + ":" + v; };
  field(r.item || "");
  field(r.t || "");
  field(r.ts || "");
  field(r.content || "");
  field(r.reasoning || "");
  field(r.is_error ? "1" : "0");
  field(r.tool_call_id || "");
  field(r.tool_name || "");
  field(r.error || "");
  s += (r.tool_calls || []).length + "#";
  for (const call of r.tool_calls || []) {
    field(call.name || "");
    field(call.arguments || "");
    field(call.id || "");
  }
  field(r.provider || "");
  field(r.model || "");
  field(r.cwd || "");
  return s;
}

export async function chainGenesis(sessionID) {
  return chainGenesisAt(sessionID, "");
}

// The active segment's genesis (sai design §14.4): a segment beginning
// after a compaction ([session.created, context.compacted, …]) seeds
// from the compaction's ItemID — derivable at truncation time and at
// fresh Open alike (it IS the segment's second record).
export async function chainGenesisAt(sessionID, compactedItemID) {
  const seed = compactedItemID ? sessionID + "/" + compactedItemID : sessionID;
  return sha256Hex(chainVersion + seed);
}

export async function chainStep(prev, r) {
  return sha256Hex(prev + "\n" + chainInput(r));
}
