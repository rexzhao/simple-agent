// The render-layer check: drives the SSR render probe (built by
// TestRenderFixture via `vite build --ssr`) against a live server —
// the fourth layer of the blank-view evidence chain (server snapshot /
// store / convert+validator / RENDER). Asserts the conversation text
// actually lands in the rendered Thread HTML.
//
// Usage: node render_check.mjs <base-url> <session-id> <needle>
// (needle = a text that must appear in the rendered HTML).
const base = process.argv[2];
const sid = process.argv[3];
const needle = process.argv[4];
if (!base || !sid || !needle) {
  console.error("render_check: missing args (base-url session-id needle)");
  process.exit(2);
}

const { renderProbe } = await import("../../../clients/chat/.render-dist/render-check-entry.js");
const out = await renderProbe(base, sid);

const failures = [];
if (out.messages === 0) failures.push("no messages converted");
if (!out.html.includes(needle)) failures.push("needle " + JSON.stringify(needle) + " not in rendered HTML");
if (out.htmlLength < 1000) failures.push("suspiciously small render: " + out.htmlLength);

if (failures.length) {
  console.error("render_check: FAILED");
  for (const f of failures) console.error("  - " + f);
  process.exit(1);
}
console.log(`render_check: OK (records=${out.records} messages=${out.messages} html=${out.htmlLength} needle present)`);
