// CI cross-implementation check: reads Go-generated fixtures from stdin
// (one JSON object per line: {record, input, digest}) and asserts this
// module's chain encoding and folding match byte for byte. Run by
// TestJSChainFixture via `node chain_check.mjs`; the chain code itself
// lives in chain.mjs — the exact module the page imports.
import { chainGenesis, chainGenesisAt, chainStep, chainInput } from "../static/chain.mjs";

const crypto = globalThis.crypto;
if (!crypto || !crypto.subtle) {
  // Node 18 and later expose WebCrypto as globalThis.crypto; refuse to
  // silently test nothing on older runtimes.
  console.error("chain_check: WebCrypto unavailable (need node >= 18)");
  process.exit(2);
}

let genesis = null;
let sessionID = "";
let prev = null;

const lines = (await import("node:fs")).readFileSync(0, "utf8").split("\n").filter(l => l.trim());
for (const line of lines) {
  const fixture = JSON.parse(line);
  if (fixture.session) {
    sessionID = fixture.session;
    // A segment line (§14.4): after a compaction the chain re-genesis
    // from the compaction's ItemID; without one it is segment 0.
    genesis = fixture.compacted
      ? await chainGenesisAt(sessionID, fixture.compacted)
      : await chainGenesis(sessionID);
    if (genesis !== fixture.genesis) {
      console.error(`genesis mismatch: js=${genesis} go=${fixture.genesis}`);
      process.exit(1);
    }
    prev = genesis;
    continue;
  }
  const gotInput = chainInput(fixture.record);
  if (gotInput !== fixture.input) {
    console.error(`chainInput mismatch on ${fixture.record.item}:\n  js=${JSON.stringify(gotInput)}\n  go=${JSON.stringify(fixture.input)}`);
    process.exit(1);
  }
  const step = await chainStep(prev, fixture.record);
  if (step !== fixture.digest) {
    console.error(`digest mismatch on ${fixture.record.item}: js=${step} go=${fixture.digest}`);
    process.exit(1);
  }
  prev = step;
}
console.log(`chain_check: ${lines.length - 1} records OK (session ${sessionID})`);
