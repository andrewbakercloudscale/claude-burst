// Runs the dashboard (internal/admin/admin.html) in headless Chromium against
// a live admin server and fails on any uncaught page error. Driven by
// TestDashboardRunsInABrowser, which starts the server and passes its URL.
//
//   node scripts/ci/admin-smoke.js <url> <expected text in #roList>
//
// Playwright is resolved from NODE_PATH, so it need not be a dependency of
// this repository; CI installs it into a temporary directory.
const { chromium } = require("playwright");

(async () => {
  const [url, want] = process.argv.slice(2);
  const browser = await chromium.launch();
  const page = await browser.newPage({ viewport: { width: 1400, height: 1000 } });
  const errors = [];
  page.on("pageerror", e => errors.push("pageerror: " + e.message));
  page.on("response", r => {
    if (r.url().includes("/api/") && r.status() >= 500) errors.push(`HTTP ${r.status()} from ${r.url()}`);
  });
  const report = { errors, console: [] };
  // Console errors are reported, not failed on: the page logs a fetch it
  // retries. They usually name the cause when a wait below times out.
  page.on("console", m => { if (m.type() === "error") report.console.push(m.text()); });
  try {
    await page.goto(url, { waitUntil: "load" });
    // The page polls for ever, so wait for what the canned state should
    // render rather than for the network to go quiet.
    await page.waitForFunction(w => {
      const el = document.getElementById("roList");
      return el && el.textContent.includes(w);
    }, want, { timeout: 15000 });
    await page.waitForTimeout(1500); // let the other first renders land
    report.roList = (await page.textContent("#roList")).replace(/\s+/g, " ").trim();
    report.permissions = (await page.textContent("#sec-permissions")).replace(/\s+/g, " ").trim().slice(0, 1500);
    report.compactSessions = (await page.textContent("#compactSessions")).replace(/\s+/g, " ").trim().slice(0, 200);
    report.title = await page.title();
  } catch (e) {
    errors.push("smoke: " + e.message.split("\n")[0]);
  }
  await browser.close();
  console.log(JSON.stringify(report));
  process.exit(errors.length ? 1 : 0);
})();
