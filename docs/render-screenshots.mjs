// Takes the dashboard screenshots in docs/screenshots.
//
// The pictures are the real dashboard, photographed with Playwright, but not
// this Mac's own: every answer from /api is rewritten on its way to the page.
// Repository names, home paths, session tasks and Wi-Fi names are replaced
// with examples, and the Issues and Intelligent Compaction tables and the two
// Context inspectors are made up whole. The figures (requests, tokens,
// spend) are the running gateway's.
//
//   PLAYWRIGHT=/path/to/node_modules/playwright node docs/render-screenshots.mjs [name ...]
//
// (PLAYWRIGHT is only needed when `playwright` is not resolvable from here.)
// With names, only those pictures are taken: see SHOTS at the foot.
//
// Nothing is written unless every picture passes the check at the end of
// shoot(): the text drawn in it must not hold this Mac's user name, one of
// its repositories, or a word from SHOT_DENY (comma separated, for whatever
// else is yours: a surname, a company, a Wi-Fi name). A picture that fails is
// named with what was found, and the run exits 1.

import { createRequire } from 'node:module'
import { writeFileSync } from 'node:fs'
import { userInfo } from 'node:os'
import { basename, dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const here = dirname(fileURLToPath(import.meta.url))
const require = createRequire(import.meta.url)
const { chromium } = require(process.env.PLAYWRIGHT || 'playwright')

// SHOT_OUT takes the pictures somewhere else, to look at them first.
const OUT = process.env.SHOT_OUT || join(here, 'screenshots')
const DASH = (process.env.BURST_DASHBOARD || 'http://127.0.0.1:7788').replace(/\/+$/, '')
const USER = userInfo().username
const HOME = '/Users/' + USER
const CODE = '/Users/you/code'
// Repositories that keep their names: this one and its public companions.
const KEEP = new Set(['claude-burst', 'claude-code-cost-sidebar'])
const EXAMPLES = ['web-shop', 'data-pipeline', 'mobile-app', 'docs-site', 'design-system', 'infra', 'cli-tools', 'search-service', 'auth-service', 'analytics', 'notebooks', 'marketing-site', 'billing', 'ios-client', 'reports', 'sandbox']
const TASKS = ['add retries to the order export', 'fix the flaky checkout test', 'move the nightly job to the new queue', 'tidy the release notes', 'profile the slow search query']

const api = async (path) => {
  try {
    const r = await fetch(DASH + path)
    return r.ok ? await r.json() : null
  } catch (err) {
    return null
  }
}

// Every repository the gateway knows by path: found by walking its answers
// for absolute paths under the home folder that sit in a field for one.
const roots = new Set()
const collect = (o, key = '') => {
  if (Array.isArray(o)) return o.forEach((v) => collect(v, key))
  if (o && typeof o === 'object') return Object.entries(o).forEach(([k, v]) => collect(v, k))
  if (typeof o === 'string' && o.startsWith(HOME + '/') && /^(repo_root|root|path|cwd|repo_dir|repos|source)$/.test(key) && !o.includes('/.')) roots.add(o)
}
for (const p of ['/api/state', '/api/intelligent-compaction', '/api/coordination?days=14', '/api/permissions', '/api/usage?range=30d&limit=200&offset=0', '/api/mod-status', '/api/handover-audit']) collect(await api(p))
if (!roots.size) {
  console.error('no answer from the dashboard at ' + DASH)
  process.exit(1)
}
const names = [...new Set([...roots].map((r) => basename(r)))].filter((n) => !KEEP.has(n) && !/^(Desktop|Documents|Downloads)$/.test(n)).sort((a, b) => b.length - a.length || a.localeCompare(b))
const alias = new Map(names.map((n, i) => [n, i < EXAMPLES.length ? EXAMPLES[i] : 'project-' + (i + 1)]))
// The folders repositories sit in, longest first, so each becomes ~/code.
const parents = [...new Set([...roots].map((r) => dirname(r)))].filter((d) => d !== HOME).sort((a, b) => b.length - a.length)
const esc = (s) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')

function plain(text) {
  for (const d of parents) text = text.split(d + '/').join(CODE + '/').split(d).join(CODE)
  // Claude Code's project folders spell a path with dashes.
  for (const d of parents) text = text.split(d.replace(/\//g, '-')).join(CODE.replace(/\//g, '-'))
  text = text.split(HOME).join('/Users/you').split(HOME.replace(/\//g, '-')).join('-Users-you')
  for (const [real, fake] of alias) text = text.replace(new RegExp('(^|[^A-Za-z0-9_.-])' + esc(real) + '(?![A-Za-z0-9_-])', 'g'), '$1' + fake)
  return text
}

// Fields that are somebody's words or surroundings, whatever they hold.
function fields(o, n = { task: 0 }) {
  if (Array.isArray(o)) return o.forEach((v) => fields(v, n))
  if (!o || typeof o !== 'object') return
  for (const [k, v] of Object.entries(o)) {
    if (k === 'task' && typeof v === 'string' && v) o[k] = TASKS[n.task++ % TASKS.length]
    else if (k === 'hotspot' && v && typeof v === 'object') Object.assign(v, { ssid: v.ssid ? 'My iPhone' : v.ssid, known: ['Home Wi-Fi', 'My iPhone'], events: [] })
    else if (k === 'signing' && v && typeof v === 'object' && typeof v.identifier === 'string') v.identifier = 'com.example.claude-burst'
    else fields(v, n)
  }
}

const SID = { a: '4c1e9a02', b: '9f3d77b1', c: 'e27a5c40' }
const issue = (at, session, files, resolved = true) => ({ at, kind: 'stopped', session, files, resolved, text: session + ' stopped with uncommitted ' + files.join(', ') })
const ISSUES = [
  issue('2026-10-01 18:08:13', SID.a, [CODE + '/web-shop/src/checkout/retry.ts', CODE + '/web-shop/src/checkout/retry.test.ts']),
  issue('2026-10-01 17:27:28', SID.b, [CODE + '/claude-burst/HANDOFF.md']),
  issue('2026-10-01 17:14:09', SID.c, [CODE + '/data-pipeline/jobs/nightly.py', CODE + '/data-pipeline/jobs/queue.py', CODE + '/data-pipeline/tests/test_nightly.py']),
  issue('2026-10-01 17:11:34', SID.a, [CODE + '/web-shop/src/orders/export.ts']),
]
const SESSIONS = [
  { id: SID.a + '-0000-4000-8000-000000000001', label: 'web-shop', task: 'add retries to the order export', cwd: CODE + '/web-shop', active_ago_s: 40 },
  { id: SID.b + '-0000-4000-8000-000000000002', label: 'claude-burst', task: '', cwd: CODE + '/claude-burst', active_ago_s: 310 },
  { id: SID.c + '-0000-4000-8000-000000000003', label: 'data-pipeline', task: 'move the nightly job to the new queue', cwd: CODE + '/data-pipeline', active_ago_s: 5400 },
]

const none = { summary_failed: 0, unused: 0, unpaid: 0, ended: 0, attempts: 0, rate: 0, lost_usd: 0 }
const learnt = (o) => ({ root: CODE + '/' + o.name, learned_at: '2026-10-06T09:11:44+02:00', stepped_on: '2026-10-06', turns_after: 120, ...o, failures: { ...none, ...o.failures } })
const INTELLIGENT = {
  mode: 'intelligent', enabled: true, fixed: 300000, floor: 100000, buffer_percent: 20, delay_minutes: 30, window_days: 14,
  repos: [
    learnt({ name: 'web-shop', threshold: 160000, target: 144000, previous: 178000, in_force: 160000, source: 'learned', compactions: 22, after_tokens: 60000, growth_per_turn: 1200, cost_usd: 0.35, payback_turns: 14, saved_per_turn_usd: 0.0031, failures: { attempts: 22 }, reason: 'a compaction leaves 60k and costs $0.35, the context grows 1.2k a turn: cheapest at 120k, plus the 20% buffer: 144k' }),
    learnt({ name: 'data-pipeline', threshold: 294000, target: 294000, previous: 270000, in_force: 294000, source: 'learned', compactions: 18, after_tokens: 85000, growth_per_turn: 3100, cost_usd: 0.52, payback_turns: 21, saved_per_turn_usd: 0.0012, failures: { attempts: 18, unpaid: 2, rate: 0.11, lost_usd: 0.71 }, reason: 'a compaction leaves 85k and costs $0.52, the context grows 3.1k a turn: cheapest at 202k, plus the 20% buffer: 243k, plus 10% for each of the 2 compactions that lost money: 294k' }),
    learnt({ name: 'mobile-app', threshold: 120000, target: 106000, previous: 133000, in_force: 120000, source: 'learned', compactions: 9, after_tokens: 48000, growth_per_turn: 800, cost_usd: 0.24, payback_turns: 11, saved_per_turn_usd: 0.0027, failures: { attempts: 9 }, reason: 'a compaction leaves 48k and costs $0.24, the context grows 0.8k a turn: cheapest at 89k, plus the 20% buffer: 106k' }),
    learnt({ name: 'docs-site', threshold: 0, target: 0, in_force: 300000, source: 'fixed', compactions: 2, after_tokens: 52000, growth_per_turn: 900, cost_usd: 0.21, payback_turns: 0, saved_per_turn_usd: 0, failures: { attempts: 2 }, reason: '2 of the 3 compactions needed in the last 14 days: on the fixed Compact at until then' }),
  ],
}

// The Context inspector shows prompts, replies and file contents, so nothing
// of a real session is used: one example session for each engine.
const item = (group, name, tokens, preview, o = {}) => ({ group, name, tokens, bytes: Math.round(tokens * 2.7), preview, ...o })
const rm = (id) => ({ id, removable: true })
const INSPECT = {
  claude: {
    session: SID.a + '-0000-4000-8000-000000000001', repo: 'web-shop', repo_root: CODE + '/web-shop', model: 'claude-opus-5-5', at: '2026-10-07T10:42:18+02:00', context: 148600, estimate: false, prompts: 14, flagged: 3,
    items: [
      item('System prompt', 'System prompt, part 1', 5700, 'You are Claude Code, Anthropic\'s official CLI for Claude.'),
      item('Built-in tools', '16 tools', 27000, 'Agent, AskUserQuestion, Bash, Edit, Glob, Grep, Read, Skill, TodoWrite, WebFetch, WebSearch, Write'),
      item('MCP tools', 'issue-tracker: 26 tools', 11800, 'create_issue, create_pull_request, get_file_contents, list_commits, search_code'),
      item('Instruction files', '/Users/you/.claude/CLAUDE.md', 800, '# Global rules: answer first, keep it short', { turn: 1, turns_ago: 13, ...rm('a1') }),
      item('Instruction files', CODE + '/web-shop/CLAUDE.md', 3900, '# web-shop: run `make test` before every commit', { turn: 1, turns_ago: 13, ...rm('a2') }),
      item('Instruction files', CODE + '/shared-rules/CLAUDE.md', 6200, '# Rules shared by every service in the monorepo', { turn: 1, turns_ago: 13, flags: ['from outside this repository'], ...rm('a3') }),
      item('Skills', '9 skills', 2100, 'code-review, deploy, release-notes, security-review', { turn: 1, turns_ago: 13, ...rm('a4') }),
      item('Your prompts', 'Prompt', 120, 'add retries to the order export, three attempts with backoff', { turn: 1, turns_ago: 13 }),
      item("Claude's replies", 'Reply', 6400, 'I will start by reading the export job and its tests.', { turn: 1, turns_ago: 13 }),
      item('Tool results', 'Read src/orders/export.ts', 9200, 'import { Queue } from "../queue"', { turn: 1, turns_ago: 13, flags: ['read again at prompt 9: this copy is out of date'], ...rm('a5') }),
      item('Tool results', 'Bash make test', 31400, '> web-shop@4.2.0 test  PASS src/orders/export.test.ts (212 tests)', { turn: 2, turns_ago: 12, flags: ['31k tokens from 12 prompts ago'], ...rm('a6') }),
      item('Tool results', 'Grep "retry" src/', 4800, 'src/checkout/retry.ts:14: export function retry<T>(', { turn: 4, turns_ago: 10, ...rm('a7') }),
      item("Claude's replies", 'Reply', 8900, 'The export job has no retry at all: a failed page ends the run.', { turn: 4, turns_ago: 10 }),
      item('Your prompts', 'Prompt', 90, 'yes, and make the backoff configurable', { turn: 9, turns_ago: 5 }),
      item('Tool results', 'Read src/orders/export.ts', 9600, 'import { Queue } from "../queue"', { turn: 9, turns_ago: 5, ...rm('a8') }),
      item("Claude's replies", 'Reply', 7700, 'Done: three attempts, backoff from ORDER_EXPORT_BACKOFF_MS.', { turn: 9, turns_ago: 5 }),
      item('Tool results', 'Bash make test', 12300, 'PASS src/orders/export.test.ts (218 tests)', { turn: 14, ...rm('a9') }),
      item('Other reminders', 'Todo list reminder', 600, 'The todo list is empty.', { turn: 14, ...rm('a10') }),
    ],
  },
  codex: {
    session: SID.c + '-0000-4000-8000-000000000003', repo: 'data-pipeline', repo_root: CODE + '/data-pipeline', model: 'gpt-5.2-codex', at: '2026-10-07T10:51:02+02:00', context: 86400, estimate: false, prompts: 8, flagged: 1,
    items: [
      item('Instructions', "Codex's instructions", 6900, 'You are Codex, a coding agent running in the Codex CLI.'),
      item('Tools', '11 tools', 5200, 'shell, apply_patch, update_plan, view_image, web_search'),
      item('AGENTS.md', CODE + '/data-pipeline/AGENTS.md', 2400, '# data-pipeline: jobs live in jobs/, tests in tests/', { turn: 1, turns_ago: 7, ...rm('c1') }),
      item('Skills', '4 skills', 900, 'release, migrate, profile, lint', { turn: 1, turns_ago: 7, ...rm('c2') }),
      item('Your prompts', 'Prompt', 140, 'move the nightly job to the new queue', { turn: 1, turns_ago: 7 }),
      item("Codex's replies", 'Reply', 5100, 'Reading jobs/nightly.py and the queue client first.', { turn: 1, turns_ago: 7 }),
      item('Tool calls', 'shell: cat jobs/nightly.py', 300, 'cat jobs/nightly.py', { turn: 1, turns_ago: 7 }),
      item('Tool outputs', 'shell: pytest -q', 38200, '412 passed, 3 skipped in 41.80s', { turn: 2, turns_ago: 6, flags: ['38k tokens from 6 prompts ago'], ...rm('c3') }),
      item('Tool outputs', 'shell: cat jobs/nightly.py', 7400, 'from pipeline.queue import LegacyQueue', { turn: 1, turns_ago: 7, ...rm('c4') }),
      item('Reasoning', 'Encrypted reasoning', 12800, '(encrypted: size only)', { turn: 5, turns_ago: 3 }),
      item("Codex's replies", 'Reply', 7060, 'The job now enqueues through QueueV2 and the old path is removed.', { turn: 8 }),
    ],
  },
}
function inspect(url) {
  const d = INSPECT[url.searchParams.get('engine') === 'codex' ? 'codex' : 'claude']
  if (url.searchParams.get('session')) return JSON.stringify(d)
  return JSON.stringify([{ session: d.session, repo: d.repo, model: d.model, at: d.at, messages: d.items.length, bytes: d.context * 3 }])
}

// What the page is given for one /api answer.
function rewrite(path, text, url) {
  if (path === '/api/intelligent-compaction') return JSON.stringify(INTELLIGENT)
  if (path === '/api/inspect') return inspect(url)
  let doc
  try {
    doc = JSON.parse(plain(text))
  } catch (err) {
    return plain(text)
  }
  fields(doc)
  if (path === '/api/coordination' && doc && doc.metrics) {
    doc.metrics.issues = ISSUES
    doc.metrics.unresolved = 0
    if (doc.status) doc.status.sessions = SESSIONS
    doc.activity = []
  }
  return JSON.stringify(doc)
}

const DENY = [USER, ...names, ...parents.map((d) => d.slice(HOME.length + 1)), ...(process.env.SHOT_DENY || '').split(',')].map((s) => s.trim().toLowerCase()).filter((s) => s.length > 2)
const found = (text) => {
  const t = text.toLowerCase()
  return DENY.filter((w) => t.includes(w))
}

const browser = await chromium.launch()
const context = await browser.newContext({ viewport: { width: 1360, height: 900 }, deviceScaleFactor: 2, locale: 'en-US' })
await context.route('**/api/**', async (route) => {
  const req = route.request()
  if (req.method() !== 'GET') return route.abort()
  try {
    const r = await route.fetch()
    const url = new URL(req.url())
    // A made-up session is one the gateway does not have: its 404 must not
    // reach the page with the example's body.
    await route.fulfill({ response: r, status: url.pathname === '/api/inspect' ? 200 : r.status(), body: rewrite(url.pathname, await r.text(), url) })
  } catch (err) {
    await route.abort()
  }
})

async function open(tab) {
  const page = await context.newPage()
  await page.goto(DASH + '/')
  // The sections fill as their answers arrive: wait for the last of them.
  await page.waitForLoadState('networkidle').catch(() => {})
  await page.waitForTimeout(2500)
  if (tab) {
    await page.click(`button.tab[data-tab="${tab}"]`)
    await page.waitForTimeout(1500)
  }
  return page
}

// One picture: the box around the elements named, top of the first to the
// bottom of the last, or down to the top of the element `until` names.
const failed = []
async function shoot(page, name, selectors, { until = '', prepare } = {}) {
  if (prepare) await page.evaluate(prepare)
  // Pictures below the fold are lazy, and a clip does not scroll to them.
  await page.evaluate(() => Promise.all([...document.images].map((i) => {
    i.loading = 'eager'
    return i.complete ? null : new Promise((done) => { i.onload = i.onerror = done })
  })))
  const box = await page.evaluate(({ selectors, until }) => {
    const els = selectors.map((s) => document.querySelector(s)).filter(Boolean)
    if (els.length !== selectors.length) return null
    const r = els.map((e) => e.getBoundingClientRect())
    const top = Math.min(...r.map((x) => x.top)) + scrollY
    const bottom = Math.max(...r.map((x) => x.bottom)) + scrollY
    const left = Math.min(...r.map((x) => x.left)) + scrollX
    const right = Math.max(...r.map((x) => x.right)) + scrollX
    const stop = until && document.querySelector(until)
    const h = (stop ? stop.getBoundingClientRect().top + scrollY - 12 : bottom) - top
    // The text inside the box, for the check.
    const text = [...document.querySelectorAll('body *')].filter((e) => !e.children.length || e.tagName === 'TD' || e.tagName === 'P').filter((e) => {
      const b = e.getBoundingClientRect()
      return b.width && b.height && b.bottom + scrollY > top && b.top + scrollY < top + h && b.right + scrollX > left && b.left + scrollX < right
    }).map((e) => (e.value !== undefined && typeof e.value === 'string' ? e.value + ' ' : '') + (e.innerText || '')).join('\n')
    return { x: left, y: top, width: right - left, height: h, text }
  }, { selectors, until })
  if (!box) return failed.push(name + ': not on the page: ' + selectors.join(', '))
  const hits = found(box.text)
  if (hits.length) {
    // With where: the words either side say which field still leaks.
    const at = box.text.toLowerCase().indexOf(hits[0])
    return failed.push(name + ': shows ' + [...new Set(hits)].join(', ') + ' ("' + box.text.slice(Math.max(0, at - 60), at + 40).replace(/\s+/g, ' ') + '")')
  }
  const png = await page.screenshot({ fullPage: true, clip: { x: box.x, y: box.y, width: box.width, height: box.height } })
  writeFileSync(join(OUT, name + '.png'), png)
  console.log(name + '.png  ' + Math.round(box.width) + 'x' + Math.round(box.height))
}

// The inspector's flagged group opens by itself; one more shows the items.
const openGroups = () => {
  const g = [...document.querySelectorAll('details.ingrp')].find((d) => d.dataset.g === 'Tool results' || d.dataset.g === 'Tool outputs')
  if (g) g.open = true
}

const want = process.argv.slice(2)
const on = (name) => !want.length || want.includes(name)

const SHOTS = {
  claude: [
    ['overview', async (page) => {
      const text = await page.evaluate(() => document.body.innerText)
      const hits = found(text.slice(0, 4000))
      if (hits.length) return failed.push('overview: shows ' + [...new Set(hits)].join(', '))
      writeFileSync(join(OUT, 'overview.png'), await page.screenshot())
      console.log('overview.png  1360x900')
    }],
    ['analytics', (page) => shoot(page, 'analytics', ['#sec-analytics'])],
    ['spend', (page) => shoot(page, 'spend', ['#sec-models', '#sec-repos'])],
    ['daily-activity', (page) => shoot(page, 'daily-activity', ['#sec-activity'])],
    ['compaction-savings', (page) => shoot(page, 'compaction-savings', ['#sec-savings'])],
    ['compaction-strategies', (page) => shoot(page, 'compaction-strategies', ['#sec-strategies'])],
    ['usage', (page) => shoot(page, 'usage', ['#sec-usage'])],
    ['context-and-cache', (page) => shoot(page, 'context-and-cache', ['#sec-context'])],
    ['context-inspector', (page) => shoot(page, 'context-inspector', ['#sec-inspect'], { prepare: openGroups })],
    ['coordination', (page) => shoot(page, 'coordination', ['#sec-coord'])],
    ['handover', (page) => shoot(page, 'handover', ['#handoverSection'], { prepare: () => document.querySelector('#handoverSection').classList.remove('collapsed') })],
    ['pauseless-compaction', (page) => shoot(page, 'pauseless-compaction', ['#sec-compaction'], { until: '.ic-panel' })],
    // The section runs on for pages: its head, down to the mode's own panel,
    // which is the next picture. That table is ten columns, so its panel is
    // widened and none is scrolled away.
    ['intelligent-compaction', (page) => shoot(page, 'intelligent-compaction', ['.ic-panel'], {
      prepare: () => {
        const el = document.querySelector('.ic-panel')
        for (let n = el; n && n !== document.body; n = n.parentElement) n.style.maxWidth = 'none'
        el.style.width = '1815px'
        document.querySelectorAll('#icTable tr').forEach((r) => Object.assign(r.lastElementChild.style, { maxWidth: '330px', width: '330px' }))
      },
    })],
    ['failover-pricing', (page) => shoot(page, 'failover-pricing', ['#sec-failover'])],
    ['coordination-issues', (page) => shoot(page, 'coordination-issues', ['#coIssues'])],
    ['sessions-and-panel', (page) => shoot(page, 'sessions-and-panel', ['#sec-sessions', '#sec-panel'])],
  ],
  codex: [
    ['codex', (page) => shoot(page, 'codex', ['#sec-codex'])],
    ['codex-insights', (page) => shoot(page, 'codex-insights', ['#sec-codex-insights'])],
    ['codex-context', (page) => shoot(page, 'codex-context', ['#sec-codex-context'])],
    ['codex-inspector', (page) => shoot(page, 'codex-inspector', ['#sec-codex-inspect'], { prepare: openGroups })],
  ],
  general: [
    ['this-mac', (page) => shoot(page, 'this-mac', ['#sec-mac', '#sec-notify'])],
  ],
}

for (const [tab, shots] of Object.entries(SHOTS)) {
  for (const [name, take] of shots.filter(([n]) => on(n))) {
    // A page for each: one picture's widening must not reach the next.
    const page = await open(tab === 'claude' ? '' : tab)
    await take(page)
    await page.close()
  }
}
await browser.close()
if (failed.length) {
  console.error('\nNOT TAKEN:\n  ' + failed.join('\n  '))
  process.exit(1)
}
