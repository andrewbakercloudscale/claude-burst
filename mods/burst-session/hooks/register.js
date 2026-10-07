// Claude Burst inside the Claude Code session: a band above the prompt, the
// usage panel's summary in a /burst pane, and Burst's alerts and Pauseless
// Compaction's lines as toasts.
//
// Each is shown once. An alert is claimed the way the usage panels claim one
// (a directory per event id, made atomically), so the Ghostty pop-up for it
// stands aside, and a pop-up that got there first is not repeated here.
// Compaction lines are taken from the gateway's queue, the same one the
// prompt-notice hook reads, so a line toasted here is not also printed
// under the prompt. The dashboard's toasts option turns both off.
//
// Where the usage panel's sidebar is installed, the sidebar draws Burst's
// context bar, route and standing problems and the panel's own figures, so
// the band stands aside and the rows above the prompt go back to the
// transcript. /burst band puts the band back (and takes it away again).
// Nothing goes in Claude Code's status line: it draws every mod's entry as
// "⚠ claude-burst: ..." in yellow, a warning's dress on a line that said
// PRIMARY.
//
// Two sources, both already on this Mac:
// - the Burst dashboard's /api/mod: route, compaction for this session, alerts
// - the usage panel's band file: its summary rows, colours and all, so the
//   numbers here are the panel's own and nothing is computed twice

// Where the dashboard is unless config.json's admin_listen says otherwise.
const DASHBOARD_DEFAULT = '127.0.0.1:7788'
let DASHBOARD = 'http://' + DASHBOARD_DEFAULT
const PANE = 'burst'
const POLL_MS = 5000

let sid = ''
let home = ''
let burst = null // last /api/mod answer, null while the dashboard is down
let down = false
let panel = [] // the usage panel's summary rows, as parsed segments
let since = 0 // alerts at or before this were here before the session
let showBar = true // the context bar under the band; /context-bar toggles it
let band = true // the band above the prompt; off where the sidebar has it
let ready = false // false until the session's first answer: nothing is known yet, which is not down
const BAND_KEY = 'band'

// /burst-dump: the context Burst last sent for this session, item by item,
// which is what the cache holds. /burst-prune takes items out of it by a
// word. Both are the dashboard's Context inspector, asked from the session:
// the content stays in the gateway's memory and is fetched when asked for.
const DUMP = 'burst-dump'
const PRUNE = 'burst-prune'
const DUMP_ROWS = 400 // rows drawn at most: a list is narrowed, a text is cut
const DUMP_TEXT = 60000 // characters of one item shown
let dump = null // what /burst-dump last fetched: { rep, filter, item, error }

// The way out when Burst is the problem. Both are immediate, so no request
// is made: they work while the gateway is down and Claude Code cannot reach
// the model. Each runs a script install.sh put on the PATH, in a Terminal
// window, where macOS can ask for the password and the output can be read.
// Neither needs the gateway, the dashboard or the checkout.
const RESCUE = {
  revert: {
    name: 'claude-burst-revert', script: 'burst-off',
    description: 'Turn Claude Burst off and send Claude Code straight to Anthropic again. Works while the gateway is down',
    started: 'Turning Burst off in a Terminal window. Undo: /claude-burst-reinstall',
  },
  reinstall: {
    name: 'claude-burst-reinstall', script: 'burst-reinstall',
    description: 'Fetch the newest Claude Burst from GitHub and install it again. Works while the gateway is down',
    started: 'Reinstalling the newest Burst in a Terminal window',
  },
}

async function rescue($, c) {
  const path = (home || (await $.env.get('HOME')) || '') + '/.local/bin/' + c.script
  try {
    const r = await $.process.run(['open', '-a', 'Terminal', path])
    if (r && r.exitCode) throw new Error((r.stderr || 'exit ' + r.exitCode).trim())
    $.ui.toast(c.started)
  } catch (err) {
    $.ui.toast('Could not run ' + path + ' (' + err + '). Run ./install.sh in the claude-burst checkout')
  }
}

// Hand-off. Burst compacts what it sends and Claude Code keeps everything,
// so a session Burst holds at 135k sends its whole history the moment Burst
// is out of the path. The gateway leaves each summary in force in
// ~/.config/claude-burst/handoff/<session>.json; here Claude Code's own
// compaction is answered with it: no summary request, no pause.

// Held history, in tokens, from which a session that has left Burst is
// compacted with Burst's summary before its next prompt goes.
const HANDOFF_AT = 300000

// Held history from which a session is compacted that way wherever its
// requests go, unless the dashboard turns that off. Past the 1M window a
// bypass is refused as too long. It can be turned off, since it has costs
// with Burst working: the next turn is read uncached, and the replaced
// messages are out of the session Claude Code sends (its transcript file
// on disk keeps them).
const HANDOFF_ALWAYS_AT = 800000

let heldSeen = 0 // the most this session was last seen to hold, kept for when the gateway is gone
let handed = '' // the hand-off already made or refused, so it is not tried again

// The dashboard's address as the gateway was told it: admin_listen, a
// host:port on this Mac. Anything else, a missing file included, is the
// default: the mod must never be pointed at another machine by a config file.
export function dashboardURL(cfg) {
  const m = /^(127\.0\.0\.1|localhost|\[::1\]):(\d{1,5})$/.exec(String((cfg && cfg.admin_listen) || ''))
  if (!m || +m[2] < 1 || +m[2] > 65535) return 'http://' + DASHBOARD_DEFAULT
  return 'http://' + m[1] + ':' + +m[2]
}

async function readJSON($, path) {
  try {
    return JSON.parse(await $.fs.read(path))
  } catch (err) {
    return null
  }
}

// On unless the dashboard's option is off. Read from the file, not from
// /api/mod: this matters most when the gateway is not there to ask.
async function handoffOn($) {
  const m = await readJSON($, home + '/.config/claude-burst/mod.json')
  return !(m && m.handoff === false)
}

async function handoffInPathOn($) {
  const m = await readJSON($, home + '/.config/claude-burst/mod.json')
  return !(m && m.handoff_in_path === false)
}

async function readHandoff($) {
  if (!home || !sid || /[^A-Za-z0-9._-]/.test(sid)) return null
  const h = await readJSON($, home + '/.config/claude-burst/handoff/' + sid + '.json')
  return h && h.session === sid && h.lead && h.first && h.last ? h : null
}

// True when this session's requests go through Burst, false when they go
// straight to Anthropic, undefined when it cannot be told. A gateway that
// is down but still in the path is true: its requests fail, they are not
// sent whole to Anthropic.
async function inPath($) {
  let base = ''
  try { base = (await $.env.get('ANTHROPIC_BASE_URL')) || '' } catch (err) { /* unset */ }
  // Base-url mode sets it in Claude Code's settings, which this process's
  // environment may not show.
  if (base === '') {
    const set = await readJSON($, home + '/.claude/settings.json')
    base = (set && set.env && set.env.ANTHROPIC_BASE_URL) || ''
  }
  if (/\/\/(127\.0\.0\.1|localhost|\[::1\])[:/]/.test(base + '/')) return true
  if (base !== '' && !/\/\/api\.anthropic\.com/.test(base)) return undefined
  let hosts
  try { hosts = await $.fs.read('/etc/hosts') } catch (err) { return undefined }
  for (const line of String(hosts).split('\n')) {
    const f = line.replace(/#.*/, '').trim().split(/\s+/)
    if ((f[0] === '127.0.0.1' || f[0] === '::1') && f.slice(1).some((h) => h.toLowerCase() === 'api.anthropic.com')) return true
  }
  return false
}

function anchorMatches(a, m) {
  if (!a || !m || m.role !== a.role) return false
  if (a.tool) {
    return (m.toolUses || []).some((t) => t.tool_use_id === a.tool) || (m.toolResults || []).some((t) => t.tool_use_id === a.tool)
  }
  return !!a.text && a.text.length >= 16 && String(m.text || '').replace(/\s+/g, '').includes(a.text)
}

// Where the kept messages start in Claude Code's transcript, or -1. Claude
// Code may hold one message of the request as several of its own, all with
// the same role, so a message of the request is a run of one role here.
function handoffCut(h, msgs) {
  const runStart = (i) => { while (i > 0 && msgs[i - 1].role === msgs[i].role) i--; return i }
  const lastNamed = !!h.last.tool || (h.last.text || '').length >= 16
  for (let i = 1; i < msgs.length; i++) {
    let cut = -1
    if (anchorMatches(h.first, msgs[i])) {
      cut = runStart(i)
      if (cut < 1) continue
      if (!h.first.tool && lastNamed) {
        let ok = false
        for (let j = cut - 1; j >= 0 && msgs[j].role === msgs[cut - 1].role; j--) ok = ok || anchorMatches(h.last, msgs[j])
        if (!ok) continue
      }
    } else if (lastNamed && anchorMatches(h.last, msgs[i - 1]) && msgs[i].role === h.first.role && msgs[i].role !== msgs[i - 1].role) {
      // A message too short to name ("Done.", "do both") is found by what
      // it follows, and must still say what little it was known to say.
      if (!h.first.tool && h.first.text && !String(msgs[i].text || '').replace(/\s+/g, '').includes(h.first.text)) continue
      cut = i
    }
    if (cut < 1) continue
    // Tool results cannot open a conversation: their calls would be gone.
    if (msgs[cut].role === 'user' && (msgs[cut].toolResults || []).length > 0) return -1
    if (msgs.slice(0, cut).some((m) => m.text === h.lead)) return -1
    return cut
  }
  return -1
}

// A kept tool result handed back with its handle is written to the
// transcript pointing at its row from before the compaction, and a later
// claude --resume follows that link back and loads the history the
// compaction replaced. Found by Capitec's burst-lite (0.2.16), and in this
// mod's own transcripts: 50, 28 and 4 such rows after three hand-offs of one
// session. Without the handle Claude Code writes it as a new row after the
// one before it. Only its text is kept: an image in a kept tool result goes.
function asNewIfResult(m) {
  if (!m || m.handle === undefined || (m.toolResults || []).length === 0) return m
  const { handle, ...rest } = m
  return rest
}

// A session that has left Burst is compacted with the summary Burst already
// wrote, once. It runs /compact, which Claude Code queues until no turn is
// running, and not $.session.compact(): a plugin's own session.compact hook
// does not see that call, so Claude Code would write a summary of its own.
let asking = false
// A session opened again (claude --resume, --continue) that Burst had
// summarised: it is compacted once as it opens, in the path or not. Opening
// is where it can be done: a closing session is a process on its way out,
// and a window that was shut or a crash never gets to close at all.
let reopened = false

// reopenedNow is whether this is another Claude Code process than the one
// that last ran this session. A reload of the mod fires session.start too,
// in the same process, and is not a restart.
async function reopenedNow($) {
  let pid = ''
  try {
    const r = await $.process.run(['sh', '-c', 'echo $PPID'])
    pid = String((r && r.stdout) || '').trim()
  } catch (err) {
    return false
  }
  if (!/^\d+$/.test(pid)) return false
  const key = 'process:' + sid
  let was
  try { was = await $.store.get(key) } catch (err) { was = undefined }
  if (was === pid) return false
  try { await $.store.set(key, pid) } catch (err) { $.ui.log('could not note this process: ' + err) }
  return true
}
// The summary the mod's own /compact is for. Claude Code may queue that
// /compact behind a running turn, so it is kept until the compaction comes:
// one the mod asked for is never Claude Code's own, paid summary.
let asked = ''
// tookIn: this session has taken a summary of Burst's in, so with none on
// offer the reason is that one, not that Burst never wrote any.
let tookIn = false
const ALREADY = "Already compacted: Burst's latest summary is in this session and it has written nothing newer, so there was nothing to hand over"
async function leaveBurst($) {
  if (asking || !(await handoffOn($))) return
  const h = await readHandoff($)
  if (!h || h.of === handed) return
  const raw = Math.max((burst && burst.session && burst.session.raw) || 0, heldSeen, h.raw || 0)
  // full: Burst's summary went in and the session still sends its limit or
  // more, so the gateway asks for this at any size, in the path or not.
  if (raw < HANDOFF_AT && !h.full) return
  const out = (await inPath($)) === false
  const again = reopened
  reopened = false
  if (!out && !again && !h.full && (raw < HANDOFF_ALWAYS_AT || !(await handoffInPathOn($)))) return
  handed = h.of
  try {
    // Still over its limit after the summary: /compact-async-full itself.
    if (h.full) await compactFast($)
    else await handOver($, h)
  } catch (err) {
    $.ui.toast((out ? 'This session no longer goes through Burst and sends its whole history (' + kTokens(raw) + ')' : 'Claude Code holds ' + kTokens(raw) + ' of history for this session, close to more than it could send without Burst') + '. /compact shortens it with the summary Burst already wrote', { timeoutMs: TOAST_MS.warn })
  }
}

// handOver has Claude Code compact, which the session.compact hook below
// answers with h. /compact-async-full is this, typed.
async function handOver($, h) {
  asking = true
  asked = h.of
  try {
    await $.command.run({ command: 'compact', args: '' })
  } finally {
    asking = false
  }
}

const FAST = 'compact-async-full'
async function compactFast($) {
  if (asking) return
  if (!(await handoffOn($))) return $.ui.toast("/" + FAST + " is off: turn on Hand Burst's summary to Claude Code on the dashboard", { timeoutMs: TOAST_MS.warn })
  const h = await readHandoff($)
  if (!h) return $.ui.toast(tookIn ? ALREADY : 'Burst holds no summary of this session yet: nothing compacted', { timeoutMs: TOAST_MS.warn })
  handed = h.of
  try {
    await handOver($, h)
  } catch (err) {
    $.ui.toast('/' + FAST + ' could not run /compact: ' + err, { timeoutMs: TOAST_MS.warn })
  }
}

// enter is everything this mod holds about one session, started afresh.
async function enter($, id) {
  sid = id
  since = Math.floor((await $.clock.now()) / 1000)
  toastsMarked = 0
  handed = ''
  asked = ''
  tookIn = false
  reopened = false
  heldSeen = 0
}

// Only a session Burst has summarised has a hand-off waiting as it opens.
async function opened($) {
  try { reopened = (await reopenedNow($)) && (await readHandoff($)) !== null } catch (err) { reopened = false }
}

// /clear, /resume and /branch carry on under another session id with no
// session.start (found by Capitec's burst-lite, 0.2.9). Without this the
// band went on showing the session that was left, and a hand-off was looked
// for under its name. Called wherever the session is about to be used.
async function follow($) {
  let id = ''
  try { id = await $.session.id() } catch (err) { return }
  if (!id || id === sid) return
  await enter($, id)
  burst = null
  await opened($)
}

export function register(on) {
  on('session.compact', async ($, e, next) => {
    // A subagent's transcript, a precompute and a /compact with the user's
    // own instructions are Claude Code's.
    if (e.trigger === 'precompute' || e.agentId || (e.instructions && e.trigger === 'manual')) return next(e)
    await follow($)
    let h = null
    try {
      if (await handoffOn($)) h = await readHandoff($)
    } catch (err) {
      h = null
    }
    const cut = h ? handoffCut(h, e.messages || []) : -1
    // Asked for by leaveBurst and no fit: nothing is compacted. Claude Code
    // writing a summary of the whole history is the user's to ask for.
    const mine = asking || (h !== null && asked === h.of && e.trigger === 'manual')
    asked = ''
    if (cut < 1 && mine && h && String((e.messages && e.messages[0] && e.messages[0].text) || '').includes(h.lead)) return { skip: ALREADY }
    if (cut < 1 && mine) return { skip: "Burst's summary does not fit this session as Claude Code holds it, so nothing was compacted. /compact has Claude Code write its own" }
    if (cut < 1) return next(e)
    handed = h.of
    tookIn = true
    $.ui.toast("Compacted with Burst's summary: " + cut + ' messages replaced, nothing written by Claude Code', { timeoutMs: TOAST_MS.info })
    return { messages: [{ role: 'user', text: h.lead, toolUses: [] }, ...e.messages.slice(cut).map(asNewIfResult)] }
  })

  on('session.start', async ($, e, next) => {
    home = (await $.env.get('HOME')) || ''
    DASHBOARD = dashboardURL(await readJSON($, home + '/.config/claude-burst/config.json'))
    await enter($, await $.session.id())
    band = !(await hasSidebar($))
    try {
      const v = await $.store.get(BAND_KEY)
      if (typeof v === 'boolean') band = v
    } catch (err) {
      // No stored choice: the band unless the sidebar is there.
    }
    await refresh($)
    ready = true
    $.ui.invalidate('ui.render')
    await opened($)
    // An entry an earlier version of this mod pinned is taken down.
    try { await $.ui.status(undefined) } catch (err) { $.ui.log('could not clear the status line: ' + err) }
    $.clock.every(POLL_MS, async () => {
      await follow($)
      await refresh($)
      try { await leaveBurst($) } catch (err) { $.ui.log('hand-off did not run: ' + err) }
      $.ui.invalidate('ui.render')
    })
    try {
      await $.command.register({ name: 'burst', description: 'Claude Burst and usage panel for this session; /burst band puts the band above the prompt back, or takes it away', immediate: true })
    } catch (err) {
      $.ui.log('could not add /burst: ' + err)
    }
    try {
      await $.command.register({ name: 'context-bar', description: 'Show or hide the context bar: what the context Burst sends is made of', immediate: true })
    } catch (err) {
      $.ui.log('could not add /context-bar: ' + err)
    }
    try {
      await $.command.register({ name: FAST, description: "Compact this session with the summary Burst already wrote: no summary request, no pause. Never Claude Code's own compaction", immediate: true })
    } catch (err) {
      $.ui.log('could not add /' + FAST + ': ' + err)
    }
    try {
      await $.command.register({ name: DUMP, description: 'Everything in the context Burst last sent for this session, which is what the cache holds: /burst-dump, /burst-dump <word> to narrow it, /burst-dump <number> for one item in full', immediate: true })
    } catch (err) {
      $.ui.log('could not add /' + DUMP + ': ' + err)
    }
    try {
      await $.command.register({ name: PRUNE, description: 'Take out of this session\'s context every tool result and instruction file a word names (a repository, a file): /burst-prune <word>, or stale, results, undo', immediate: true })
    } catch (err) {
      $.ui.log('could not add /' + PRUNE + ': ' + err)
    }
    for (const c of Object.values(RESCUE)) {
      try {
        await $.command.register({ name: c.name, description: c.description, immediate: true })
      } catch (err) {
        $.ui.log('could not add /' + c.name + ': ' + err)
      }
    }
    return next(e)
  })

  for (const c of Object.values(RESCUE)) {
    on('command.run', { command: c.name }, async ($) => {
      await rescue($, c)
      return {}
    })
  }

  on('command.run', { command: 'burst' }, async ($, e) => {
    if (String((e && e.args) || '').trim().toLowerCase() === 'band') {
      band = !band
      try { await $.store.set(BAND_KEY, band) } catch (err) { $.ui.log('could not save the band choice: ' + err) }
      $.ui.invalidate('ui.render')
      $.ui.toast(band ? 'Burst band above the prompt' : 'Burst band off: the sidebar has it')
      return {}
    }
    await $.ui.open({ id: PANE, title: 'Burst', focus: true, closeOnEscape: true })
    return {}
  })

  on('command.run', { command: FAST }, async ($) => {
    await follow($)
    // Not awaited: Claude Code may hold the /compact until the turn ends.
    compactFast($).catch((err) => $.ui.log('/' + FAST + ' did not run: ' + err))
    return {}
  })

  on('command.run', { command: DUMP }, async ($, e) => {
    await follow($)
    await loadDump($, e && e.args)
    await $.ui.open({ id: DUMP, title: 'Burst context', focus: true, closeOnEscape: true })
    $.ui.invalidate('ui.render')
    return {}
  })

  on('command.run', { command: PRUNE }, async ($, e) => {
    await follow($)
    $.ui.toast(await prune($, e && e.args), { timeoutMs: TOAST_MS.error })
    return {}
  })

  on('command.run', { command: 'context-bar' }, async ($) => {
    showBar = !showBar
    $.ui.invalidate('ui.render')
    return {}
  })

  on('ui.render', { component: 'AbovePrompt' }, async ($, e, next) => {
    if (!ready || !band) return next(e)
    const { Box, Text } = $.ui.resolve(e)
    const theirs = await next(e)
    const rows = [Box({ flexDirection: 'row', columnGap: 1, children: bandSegments(Text) })]
    if (showBar) rows.push(...contextBar(Box, Text, e.viewport && e.viewport.columns))
    const ours = rows.length === 1 ? rows[0] : Box({ flexDirection: 'column', children: rows })
    return theirs ? Box({ flexDirection: 'column', children: [theirs, ours] }) : ours
  })

  on('ui.render', { component: 'Pane' }, async ($, e, next) => {
    if (e.requestId === DUMP) {
      const { Box, Text } = $.ui.resolve(e)
      return Box({ flexDirection: 'column', children: dumpRows(Text) })
    }
    if (e.requestId !== PANE) return next(e)
    const { Box, Text, Button } = $.ui.resolve(e)
    const rows = []
    rows.push(Text({ bold: true, children: ['Usage panel'] }))
    if (panel.length === 0) {
      rows.push(Text({ dimColor: true, children: ['  no summary yet: the panel writes one on its slow refresh'] }))
    }
    for (const line of panel) rows.push(lineText(Text, line))
    rows.push(Text({ children: [' '] }))
    rows.push(Text({ bold: true, children: ['Claude Burst'] }))
    for (const [k, v, c] of burstRows()) {
      rows.push(Text({ children: ['  ' + k + ': ', Text({ color: c, children: [v] })] }))
    }
    rows.push(Text({ children: [' '] }))
    rows.push(Button({
      key: 'dashboard', label: 'd: Open the dashboard', hotkey: 'd', plain: true,
      onPress: async () => {
        try { await $.process.run(['open', DASHBOARD]) } catch (err) { $.ui.toast('could not open the dashboard') }
      },
    }))
    return Box({ flexDirection: 'column', children: rows })
  })
}

async function loadDump($, args) {
  const a = String(args || '').trim()
  dump = { rep: null, filter: '', item: null, error: '' }
  try {
    const r = await $.http.fetch(DASHBOARD + '/api/inspect?session=' + encodeURIComponent(sid))
    if (r.status === 404) {
      await wantHistory($)
      throw new Error('Burst has not seen this session\'s whole conversation since the gateway started: each request carries only what is new. It is asked for with the next request, so ask again after the next reply')
    }
    if (!r.ok) throw new Error('the dashboard answered ' + r.status)
    dump.rep = JSON.parse(r.text)
    if (dump.rep.since > 0) await wantHistory($)
    if (!/^\d+$/.test(a)) {
      dump.filter = a.toLowerCase()
      return
    }
    const it = (dump.rep.items || [])[+a - 1]
    if (!it) throw new Error('no item ' + a + ': this context has ' + (dump.rep.items || []).length)
    const t = await $.http.fetch(DASHBOARD + '/api/inspect-item?session=' + encodeURIComponent(sid) + '&i=' + (+a - 1))
    if (!t.ok) throw new Error('the session moved on while asking for item ' + a + ': ask again')
    dump.item = { n: +a, it, text: String(t.text || '') }
  } catch (err) {
    dump.error = String((err && err.message) || err)
  }
}

// A session on a message thread sends only what is new, so the gateway has
// the conversation as it was last sent whole. This has it asked for with
// the session's next request.
async function wantHistory($) {
  try {
    await $.http.fetch(DASHBOARD + '/api/inspect/refresh', {
      method: 'POST',
      headers: { 'X-Claude-Burst-Admin': '1', 'Content-Type': 'application/json' },
      body: JSON.stringify({ session: sid }),
    })
  } catch (err) {
    // The list is still shown, with how far behind it is.
  }
}

// What an item is, in a word, for the list's third column.
const DUMP_KIND = {
  'System prompt': 'system', 'Built-in tools': 'tools', 'MCP tools': 'mcp', 'Instruction files': 'file', Skills: 'skills',
  'Other reminders': 'note', 'Your prompts': 'prompt', "Claude's replies": 'reply', 'Tool results': 'result',
}

function tokens(n) {
  return n < 1000 ? String(n) : n < 10000 ? (n / 1000).toFixed(1) + 'k' : kTokens(n)
}

// Text as a pane can draw it: no escape codes, no control characters.
function plain(s) {
  return String(s).replace(/\x1b\[[0-9;?]*[A-Za-z]/g, '').replace(/\t/g, '  ').replace(/[\x00-\x09\x0b-\x1f\x7f]/g, '')
}

function dumpRows(Text) {
  const dim = (t) => Text({ dimColor: true, children: [t] })
  if (!dump) return [dim('/' + DUMP + ' has not been asked for in this session')]
  if (dump.error) return [Text({ color: 'red', children: [dump.error] })]
  const rep = dump.rep
  const items = rep.items || []
  const at = new Date(rep.at)
  const when = String(at.getHours()).padStart(2, '0') + ':' + String(at.getMinutes()).padStart(2, '0')
  const total = items.reduce((n, it) => n + it.tokens, 0)
  const rows = []
  if (dump.item) {
    const { n, it, text } = dump.item
    rows.push(Text({ bold: true, children: ['Item ' + n + ': ' + it.name] }))
    rows.push(dim(it.group + ', ' + tokens(it.tokens) + ' tokens' + (it.turns_ago > 0 ? ', ' + it.turns_ago + ' prompts ago' : '') + (it.removed ? ', removed: this note is what is sent' : '') + (it.flags ? ', ' + it.flags.join(', ') : '')))
    rows.push(Text({ children: [' '] }))
    const lines = plain(text.slice(0, DUMP_TEXT)).split('\n')
    for (const l of lines.slice(0, DUMP_ROWS)) rows.push(Text({ children: [l === '' ? ' ' : l] }))
    if (text.length > DUMP_TEXT || lines.length > DUMP_ROWS) rows.push(dim('Cut here: ' + text.length + ' characters in all. The dashboard\'s Context inspector shows the rest'))
    return rows
  }
  rows.push(Text({ bold: true, children: ['What Burst last sent for this session, at ' + when + ': ' + tokens(total) + ' tokens' + (rep.estimate ? ' (estimated)' : '') + ' in ' + items.length + ' items. The cache holds this'] }))
  if (rep.since > 0) rows.push(Text({ color: 'yellow', children: [rep.since + (rep.since === 1 ? ' request' : ' requests') + ' since ' + when + ' added to it and are not listed: the session sends only what is new. The whole conversation is asked for with the next request, so ask again after the next reply'] }))
  const groups = new Map()
  for (const it of items) {
    const g = groups.get(it.group) || { tokens: 0, n: 0 }
    g.tokens += it.tokens
    g.n++
    groups.set(it.group, g)
  }
  for (const [name, g] of [...groups].sort((a, b) => b[1].tokens - a[1].tokens)) {
    rows.push(Text({ children: ['  ' + name.padEnd(18) + tokens(g.tokens).padStart(6) + String(total > 0 ? Math.round((g.tokens * 100) / total) : 0).padStart(4) + '%  ', dim(g.n + (g.n === 1 ? ' item' : ' items'))] }))
  }
  rows.push(Text({ children: [' '] }))
  const words = dump.filter.split(/\s+/).filter(Boolean)
  const shown = []
  items.forEach((it, i) => {
    const hay = (it.name + ' ' + it.group + ' ' + (it.preview || '')).toLowerCase()
    if (words.every((w) => hay.includes(w))) shown.push([i + 1, it])
  })
  if (words.length > 0) rows.push(dim(shown.length + ' items with "' + dump.filter + '" in their name or first lines, ' + tokens(shown.reduce((n, x) => n + x[1].tokens, 0)) + ' tokens'))
  for (const [n, it] of shown.slice(0, DUMP_ROWS)) {
    const said = it.group === 'Your prompts' || (it.group === "Claude's replies" && !it.name.startsWith('Call: '))
    const label = plain(said ? it.name + ': ' + (it.preview || '') : it.name).slice(0, 160)
    const note = (it.removed ? '  removed' : '') + (it.flags ? '  ' + it.flags.join(', ') : '')
    rows.push(Text({
      wrap: 'truncate-end', dimColor: it.removed || undefined, color: it.group === 'Your prompts' ? 'cyan' : undefined,
      children: [String(n).padStart(4) + tokens(it.tokens).padStart(7) + '  ' + (DUMP_KIND[it.group] || 'other').padEnd(7) + label, note ? Text({ color: 'yellow', children: [note] }) : ''],
    }))
  }
  if (shown.length > DUMP_ROWS) rows.push(dim((shown.length - DUMP_ROWS) + ' more: narrow the list with /' + DUMP + ' <word>'))
  rows.push(Text({ children: [' '] }))
  rows.push(dim('/' + DUMP + ' <number> shows one item in full, /' + DUMP + ' <word> narrows the list, /' + PRUNE + ' <word> takes out what the word names'))
  return rows
}

// prune asks the gateway to take items out, or put them back, and returns
// what to tell the user.
async function prune($, args) {
  const what = String(args || '').trim()
  if (what === '') return '/' + PRUNE + ' <word> takes out of this session\'s context every tool result and instruction file whose file, command or name has the word: a repository\'s name, a file. Also: stale (out of date copies), results (every tool result before the latest prompt), undo. /' + DUMP + ' shows what is there'
  const body = /^(undo|restore)$/i.test(what) ? { session: sid, restore: true } : { session: sid, what }
  try {
    const r = await $.http.fetch(DASHBOARD + '/api/inspect/prune', {
      method: 'POST',
      headers: { 'X-Claude-Burst-Admin': '1', 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    })
    if (r.status === 404) return 'Nothing pruned yet: Burst has not seen this session\'s whole conversation since the gateway started. It is asked for with the next request, so prune again after the next reply'
    if (!r.ok) return 'Nothing pruned: the dashboard answered ' + r.status
    return String(JSON.parse(r.text).detail || 'Done')
  } catch (err) {
    return 'Nothing pruned: the Burst dashboard is not answering'
  }
}

// The usage panel's installer leaves this marker while its sidebar mod is
// installed (the same one its Ghostty split stands aside for).
async function hasSidebar($) {
  if (!home) return false
  try {
    await $.fs.read(home + '/.config/claude-panel/mod-installed')
    return true
  } catch (err) {
    return false
  }
}

// How long each toast stays: a problem longer than news.
const TOAST_MS = { error: 20000, warn: 12000, info: 8000 }

// True when this alert is the mods' to show. The first mod to ask makes the
// panels' claim and a marker beside it; every other session's mod sees the
// marker and shows it too, each in its own window. A claim with no marker
// is a panel's: its pop-up has shown it, so no toast. An alert with no id,
// or a Mac where the claim cannot be made, is shown: better twice than not.
async function claim($, id) {
  if (!id || !home || /[^A-Za-z0-9._-]/.test(id)) return true
  const dir = home + '/.config/claude-panel/alerts-claimed'
  try {
    const r = await $.process.run(['/bin/sh', '-c', 'mkdir -p "$1" 2>/dev/null; mkdir "$1/$2" 2>/dev/null && mkdir "$1/$2.mod" 2>/dev/null; if [ -d "$1/$2.mod" ]; then exit 0; elif [ -d "$1/$2" ]; then exit 1; else exit 0; fi', 'sh', dir, id], { timeoutMs: 5000 })
    return r.exitCode === 0
  } catch (err) {
    return true
  }
}

// Tells the usage panel this session's compaction news is toasted here, so
// it does not also float "Async Compaction In Progress" over the window: a
// file named for the session, touched once a minute while toasts are on and
// removed when they are turned off. The panel ignores one three minutes old.
let toastsMarked = 0
async function markToasts($, on) {
  if (!home || !sid || /[^A-Za-z0-9._-]/.test(sid)) return
  const now = await $.clock.now()
  if (on ? now - toastsMarked < 60000 : toastsMarked === 0) return
  toastsMarked = on ? now : 0
  const script = on ? 'mkdir -p "$1" 2>/dev/null; : > "$1/$2"' : 'rm -f "$1/$2"'
  try {
    await $.process.run(['/bin/sh', '-c', script, 'sh', home + '/.config/claude-panel/mod-toasts', sid], { timeoutMs: 5000 })
  } catch (err) {
    // Unmarked, the panel floats its notice as well: twice, not never.
  }
}

// Pauseless Compaction's news for this session, each line a toast. Asking
// takes the lines from the gateway, as the prompt-notice hook does; `mod`
// tells it who asks, and it then answers the hook with nothing, so a line
// is a toast and not also red text under the prompt.
async function compactionLines($) {
  if (!sid) return
  try {
    const r = await $.http.fetch(DASHBOARD + '/api/prompt-notice', {
      method: 'POST',
      headers: { 'X-Claude-Burst-Admin': '1', 'Content-Type': 'application/json' },
      body: JSON.stringify({ session_id: sid, hook_event_name: 'PostToolUse', mod: true }),
    })
    if (!r.ok || !r.text) return
    const lines = String(JSON.parse(r.text).systemMessage || '').split('\n')
    for (const line of lines) if (line.trim() !== '') $.ui.toast(line.replace(/^\u26a1\s*/, ''), { timeoutMs: TOAST_MS.info })
  } catch (err) {
    // The gateway keeps what it could not hand over; the hook still shows it.
  }
}

async function refresh($) {
  try {
    const r = await $.http.fetch(DASHBOARD + '/api/mod?session=' + encodeURIComponent(sid) + '&since=' + since)
    if (!r.ok) throw new Error('status ' + r.status)
    burst = JSON.parse(r.text)
    if (burst.session) heldSeen = burst.session.raw || 0
    if (down) $.ui.toast('Burst gateway back')
    down = false
    for (const a of burst.alerts || []) {
      since = Math.max(since, a.ts)
      if (burst.toasts && (await claim($, a.id))) $.ui.toast('Burst: ' + a.title, { timeoutMs: TOAST_MS[a.severity] || TOAST_MS.info })
    }
    await markToasts($, !!burst.toasts)
    if (burst.toasts) await compactionLines($)
  } catch (err) {
    if (!down) $.ui.toast('Burst dashboard not answering')
    down = true
    burst = null
  }
  if (home && sid) {
    try {
      panel = parsePanel(await $.fs.read(home + '/.cache/ccusage-panel-cache/band/' + sid + '.ansi'))
    } catch (err) {
      panel = []
    }
  }
}

// The band: Burst's state, the context Burst really sends (after its own
// compaction, which Claude Code's figure does not know about), then the
// panel's Session and Today rows.
function bandSegments(Text) {
  const out = []
  if (down || !burst) {
    out.push(Text({ color: 'red', bold: true, children: ['⚡ Burst down'] }))
  } else {
    const failing = burst.primary_failing > 0
    const color = burst.overflow ? 'yellow' : failing ? 'red' : 'green'
    const route = burst.overflow ? 'SECONDARY' : failing ? 'PRIMARY failing' : 'PRIMARY'
    out.push(Text({ color, bold: true, children: ['⚡ ' + route] }))
    const s = burst.session
    if (s && s.context > 0) {
      const pct = s.compact_at > 0 ? Math.round((s.context * 100) / s.compact_at) : 0
      const c = pct >= 100 ? 'red' : pct >= 80 ? 'yellow' : undefined
      const label = 'ctx ' + kTokens(s.context) + (s.compact_at > 0 ? ' of ' + kTokens(s.compact_at) : '')
      out.push(Text({ color: c, children: [label] }))
      // Claude Code's own history, which Burst's compaction never shrinks:
      // the gap is what Burst saves each turn. Information, so dim.
      if (s.raw > 0) out.push(Text({ dimColor: true, children: ['CC holds ' + kTokens(s.raw)] }))
      if (s.state && s.state !== 'ok') out.push(Text({ color: 'cyan', children: [s.state] }))
    }
  }
  const problem = (burst && burst.problems || [])[0]
  if (problem) {
    out.push(Text({ color: problem.severity === 'error' ? 'red' : 'yellow', bold: true, children: ['⚠ ' + problem.title] }))
  }
  for (const want of ['Session:', 'Today:']) {
    const line = panel.find((l) => l.text.includes(want))
    if (line) {
      out.push(Text({ dimColor: true, children: ['·'] }))
      out.push(lineText(Text, trimLabel(line)))
    }
  }
  return out
}

// One colour per part, in the gateway's order (internal/router/context_parts.go).
const PART_COLOURS = {
  'System prompt': 'gray',
  'System tools': 'cyan',
  'MCP tools': 'magenta',
  'Memory files': 'yellow',
  'Messages': 'blue',
  'Tool results': 'green',
}

// The context bar: what the context Burst really sends is made of, as a
// stacked bar against the compaction limit (or the context itself when
// compaction is off), with a legend underneath. Nothing until a response
// from this session has reported its context.
export function contextBar(Box, Text, columns) {
  const s = burst && burst.session
  if (!s || !s.parts || s.parts.length === 0 || !(s.context > 0)) return []
  const scale = Math.max(s.compact_at || 0, s.context)
  const width = Math.max(10, Math.min(60, (columns || 80) - 30))
  const cells = []
  let used = 0
  for (const p of s.parts) {
    const n = Math.max(1, Math.round((p.tokens * width) / scale))
    const take = Math.min(n, width - used)
    if (take <= 0) break
    cells.push(Text({ color: PART_COLOURS[p.name], children: ['█'.repeat(take)] }))
    used += take
  }
  if (used < width) cells.push(Text({ dimColor: true, children: ['░'.repeat(width - used)] }))
  const pct = Math.round((s.context * 100) / scale)
  const bar = Box({
    flexDirection: 'row',
    columnGap: 1,
    children: [
      Box({ flexDirection: 'row', children: cells }),
      Text({ dimColor: true, children: [kTokens(s.context) + (s.compact_at > 0 ? ' of ' + kTokens(s.compact_at) + ' (' + pct + '%)' : '')] }),
    ],
  })
  const legend = Box({
    flexDirection: 'row',
    columnGap: 2,
    flexWrap: 'wrap',
    children: s.parts.map((p) =>
      Text({ children: [Text({ color: PART_COLOURS[p.name], children: ['■ '] }), Text({ dimColor: true, children: [p.name + ' ' + kTokens(p.tokens)] })] }),
    ),
  })
  return [bar, legend]
}

function burstRows() {
  if (down || !burst) return [['Gateway', 'dashboard not answering at ' + DASHBOARD, 'red']]
  const rows = [
    ['Route', burst.route + (burst.overflow && burst.reason ? ' (' + burst.reason + ')' : ''), burst.overflow ? 'yellow' : 'green'],
    ['Anthropic', burst.primary_failing > 0 ? burst.primary_failing + ' failures since the last answer' : 'answering', burst.primary_failing > 0 ? 'red' : 'green'],
  ]
  const s = burst.session
  if (s) {
    rows.push(['Context sent', kTokens(s.context) + (s.compact_at > 0 ? ', compacts at ' + kTokens(s.compact_at) : ', compaction off'), undefined])
    for (const p of s.parts || []) rows.push(['  ' + p.name, kTokens(p.tokens), PART_COLOURS[p.name]])
    rows.push(['Compaction', s.state, s.state === 'ok' ? undefined : 'cyan'])
  } else {
    rows.push(['Compaction', 'no request from this session yet', undefined])
  }
  for (const p of burst.problems || []) rows.push(['Problem', p.title, p.severity === 'error' ? 'red' : 'yellow'])
  rows.push(['Last 24h', burst.today_requests + ' requests, $' + burst.today_usd.toFixed(2) + ' at API prices', undefined])
  rows.push(['Version', burst.version, undefined])
  return rows
}

function kTokens(n) {
  return n >= 1000000 ? (n / 1000000).toFixed(2) + 'M' : Math.round(n / 1000) + 'k'
}

// One parsed line: its segments, each a piece of text with the colour and
// weight the panel gave it.
function lineText(Text, line) {
  return Text({
    wrap: 'truncate-end',
    children: line.segs.map((g) => Text({ color: g.color, backgroundColor: g.bg, bold: g.bold || undefined, dimColor: g.dim || undefined, children: [g.text] })),
  })
}

// "  💰 Session: $3.20, Burn $0.75/hr" becomes "Session $3.20, Burn $0.75/hr"
// for the band, where the emoji and the colon cost width.
function trimLabel(line) {
  const segs = line.segs.map((g) => ({ ...g })).filter((g) => g.text.length > 0)
  if (segs.length > 0) segs[0].text = segs[0].text.replace(/^\s*\S*\s*(\w+):\s*/u, '$1 ')
  return { text: line.text, segs }
}

// The panel's ANSI colours to Text props: 1, 2, 30-37, 90-97, and 38/48 with
// 5;n or 2;r;g;b. Anything else is dropped rather than drawn as escape codes.
export function parsePanel(raw) {
  const lines = []
  for (const l of raw.split('\n')) {
    if (l.trim() === '') continue
    const segs = []
    let st = {}
    const parts = l.split(/\x1b\[([0-9;]*)m/)
    for (let i = 0; i < parts.length; i++) {
      if (i % 2 === 1) {
        st = applySGR(st, parts[i])
      } else if (parts[i] !== '') {
        segs.push({ text: parts[i].replace(/\x1b\[[0-9;?]*[A-Za-z]/g, ''), color: st.color, bg: st.bg, bold: st.bold, dim: st.dim })
      }
    }
    lines.push({ text: segs.map((g) => g.text).join(''), segs })
  }
  return lines
}

function applySGR(st, codes) {
  const c = codes === '' ? [0] : codes.split(';').map(Number)
  let out = { ...st }
  for (let i = 0; i < c.length; i++) {
    const n = c[i]
    if (n === 0) out = {}
    else if (n === 1) out.bold = true
    else if (n === 2) out.dim = true
    else if (n === 22) { out.bold = false; out.dim = false }
    else if (n === 39) out.color = undefined
    else if (n >= 30 && n <= 37) out.color = 'ansi256(' + (n - 30) + ')'
    else if (n >= 90 && n <= 97) out.color = 'ansi256(' + (n - 90 + 8) + ')'
    else if (n === 38 && c[i + 1] === 5) { out.color = 'ansi256(' + c[i + 2] + ')'; i += 2 }
    else if (n === 38 && c[i + 1] === 2) { out.color = 'rgb(' + c[i + 2] + ',' + c[i + 3] + ',' + c[i + 4] + ')'; i += 4 }
    else if (n === 49) out.bg = undefined
    else if (n === 48 && c[i + 1] === 5) { out.bg = 'ansi256(' + c[i + 2] + ')'; i += 2 }
    else if (n === 48 && c[i + 1] === 2) { out.bg = 'rgb(' + c[i + 2] + ',' + c[i + 3] + ',' + c[i + 4] + ')'; i += 4 }
  }
  return out
}
