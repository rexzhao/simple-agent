// The real-browser UI check (C6a-fix and onward): headless Chromium
// against the BUILT dist served by a live sai. Asserts the sidebar's
// new-session entry exists, is usable with nothing selected, opens
// the dialog with the project/model selects, and that creating a
// session through it lands the user in a live one. Takes the three
// smoke screenshots (the send-审 attachment).
//
// Usage: node sidebar-check.mjs <base-url> <shots-dir>
// Run from clients/chat (playwright resolves from its node_modules).
import { chromium } from "playwright";
import { mkdirSync } from "node:fs";
import { join } from "node:path";

const base = process.argv[2];
const shots = process.argv[3];
if (!base || !shots) {
  console.error("sidebar_check: missing args (base-url shots-dir)");
  process.exit(2);
}
mkdirSync(shots, { recursive: true });

const failures = [];
function assert(cond, what) {
  if (!cond) failures.push(what);
}

// expectSettled polls the server state until the run is done — the
// composer is NOT a running indicator (a running session accepts
// steers), so the wire is the truth.
async function expectSettled(page) {
  for (let i = 0; i < 60; i++) {
    const sid = (page.url().match(/#\/s\/([a-z0-9]+)/) || [])[1];
    if (sid) {
      const st = await page.request.get(new URL("/api/s/" + sid + "/state", page.url()).toString());
      if (st.ok() && !(await st.json()).running) return;
    }
    await page.waitForTimeout(500);
  }
  throw new Error("the run never settled (state.running stayed true)");
}

let browser;
try {
  browser = await chromium.launch();
} catch (e) {
  // No browser binary (e.g. an environment that skipped the install):
  // a SKIP, not a failure — exit 3 tells the Go driver to t.Skip.
  const firstLine = String(e).split("\n")[0];
  console.error("sidebar_check: SKIP (no browser binary: " + firstLine + ")");
  process.exit(3);
}
const page = await browser.newPage({ viewport: { width: 1280, height: 800 } });
// Diagnostics: a blank page or a crashed app otherwise fails as an
// opaque locator timeout.
page.on("pageerror", e => console.error("PAGEERROR: " + String(e).slice(0, 300)));
page.on("console", m => {
  if (m.type() === "error") console.error("CONSOLE: " + m.text().slice(0, 300));
});
page.on("response", r => {
  if (r.url().includes("/api/")) console.log("API: " + r.status() + " " + r.request().method() + " " + r.url().slice(-40));
});

// 1. The empty state: the landing redirects to /chat; nothing is
// selected; the sidebar still carries the new-session entry.
await page.goto(base + "/", { waitUntil: "networkidle" });
const newBtn = page.getByRole("button", { name: "new session" });
assert((await newBtn.count()) === 1, "the sidebar's new-session button is missing");
assert(await newBtn.isVisible(), "the new-session button is not visible");
assert(await newBtn.isEnabled(), "the new-session button is disabled with no session selected");
const navText = (await page.textContent("nav")) || "";
assert(navText.includes("no sessions"), "the empty-list text is missing");
await page.screenshot({ path: join(shots, "1-no-session.png") });

// 2. Clicking it opens the dialog: project + model selects and the
// Create button.
await newBtn.click();
const dialog = page.getByRole("dialog");
await dialog.waitFor({ state: "visible", timeout: 5000 });
assert((await dialog.locator("select").count()) >= 2, "the dialog lacks the project/model selects");
assert(
  (await dialog.getByRole("button", { name: "Create", exact: true }).count()) === 1,
  "the dialog lacks the Create button",
);
// Wait out the entrance animation: a mid-animation screenshot looks
// like a broken translucent panel (a review's actual false alarm).
await page.waitForTimeout(600);
await page.screenshot({ path: join(shots, "2-dialog-open.png") });

// 3. Creating through the dialog: POST -> navigate -> the composer
// enables (the live empty session). The sidebar row needs the first
// message (the §17.8-e trade-off: empty sessions are not listed), so
// the smoke drives one full turn and then waits for the row.
await dialog.getByRole("button", { name: "Create", exact: true }).click();
// The composer enables as soon as the navigation lands; the budget
// is 20s because under load the POST/render chain exceeds a tight
// one (review reproduced 2/3 red under parallel load; 0/4 at 20s).
const composer = page.getByRole("textbox", { name: "Message input" });
await composer.waitFor({ state: "visible", timeout: 20000 });
assert(await composer.isEnabled(), "the composer is still disabled after creating a session");
await composer.fill("hello");
await composer.press("Enter");
// The reply lands and the poll (5s) picks the materialized session
// up. The row's title is the FIRST USER INPUT (the listing rule), so
// wait for the sent text — not "untitled".
const row = page.locator("nav button", { hasText: "hello" });
try {
  await row.waitFor({ state: "visible", timeout: 15000 });
} catch (e) {
  await page.screenshot({ path: join(shots, "FAIL-step3.png") });
  throw e;
}
await page.waitForTimeout(500); // let the reply settle into the shot
await page.screenshot({ path: join(shots, "3-session-created.png") });

// 4. The delete confirmation: the row's × (hover-revealed) opens the
// dialog; Cancel sends nothing and keeps the row.
await row.hover();
const delBtn = page.locator("nav button[aria-label^='delete session']").first();
await delBtn.waitFor({ state: "visible", timeout: 5000 });
await delBtn.click();
const delDialog = page.getByRole("dialog");
await delDialog.waitFor({ state: "visible", timeout: 5000 });
assert(
  ((await delDialog.textContent()) || "").includes("deleted permanently"),
  "the delete confirmation lacks the permanence warning",
);
await delDialog.getByRole("button", { name: "Cancel" }).click();
await delDialog.waitFor({ state: "hidden", timeout: 5000 });
assert(await row.isVisible(), "Cancel removed the row anyway");
await page.screenshot({ path: join(shots, "4-delete-confirm.png") });

// 5. The approval card (the C6c UI surface, really rendered): the
// second turn asks for the echo tool — the card appears with the
// tool name and arguments, Approve resolves it to "approved —
// running", and the run completes with the tool's result.
const composer2 = page.getByRole("textbox", { name: "Message input" });
await composer2.fill("run the tool");
await composer2.press("Enter");
const card = page.locator("[data-testid='approval-card']");
await card.waitFor({ state: "visible", timeout: 10000 });
const cardText = (await card.textContent()) || "";
assert(cardText.includes("echo"), "the card lacks the tool name");
assert(cardText.includes("say"), "the card lacks the arguments");
assert(cardText.includes("waiting for your decision"), "the card lacks the pending state line");
const approveBtn = card.getByRole("button", { name: "approve" });
await approveBtn.waitFor({ state: "visible", timeout: 8000 });
assert(await approveBtn.isEnabled(), "the approve button is disabled on a pending card");
await page.screenshot({ path: join(shots, "5-approval-card.png") });
await approveBtn.click();
await page
  .locator("[data-testid='approval-card']", { hasText: "approved — running" })
  .waitFor({ state: "visible", timeout: 10000 });
// The resolved card drops its buttons entirely (the outcome display
// has nothing to answer) — the approve control unmounts.
if ((await card.getByRole("button", { name: "approve" }).count()) !== 0) {
  failures.push("the approve button survived resolution");
}
await page.waitForTimeout(500);
await page.screenshot({ path: join(shots, "6-approval-resolved.png") });

// 6. Archive: the row's ⌄ parks it; the collapsed section appears
// and lists it; the working set empties. Wait for the run to SETTLE
// first — the archive refuses a running session (409 run_active) and
// the post-approval continuation is still streaming here.
await expectSettled(page);
await row.hover();
const archBtn = page.locator("nav button[aria-label^='archive session']").first();
await archBtn.waitFor({ state: "visible", timeout: 5000 });
await archBtn.click();
const section = page.locator("nav button", { hasText: "archived (" });
await section.waitFor({ state: "visible", timeout: 10000 });
await section.click(); // expand
const archivedRow = page.locator("nav button", { hasText: "hello" });
await archivedRow.waitFor({ state: "visible", timeout: 5000 });
await page.screenshot({ path: join(shots, "7-archived.png") });

// 7. The deep-link probe: an unknown session id presents the hint and
// bounces to the list.
await page.goto(base + "/#/s/s00000000000000000000000000", { waitUntil: "networkidle" });
await page.waitForTimeout(500);
const strip = (await page.textContent("body")) || "";
assert(strip.includes("session not found"), "the unknown-id deep link shows no hint");
await page.screenshot({ path: join(shots, "8-deep-link-hint.png") });

await browser.close();

if (failures.length) {
  console.error("sidebar_check: FAILED");
  for (const f of failures) console.error("  - " + f);
  process.exit(1);
}
console.log(
  `sidebar_check: OK (button, dialog, create->live, approval card approve->resolved, delete-confirm cancel, archive section, deep-link hint; shots in ${shots})`,
);
