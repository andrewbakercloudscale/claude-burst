// Claude Burst inside the Claude Code session: a band above the prompt, the
// usage panel's summary in a /burst pane, and Burst's alerts as toasts.
//
// Two sources, both already on this Mac:
// - the Burst dashboard's /api/mod: route, compaction for this session, alerts
// - the usage panel's band file: its summary rows, colours and all, so the
//   numbers here are the panel's own and nothing is computed twice

const DASHBOARD = 'http://127.0.0.1:7788'
const PANE = 'burst'
const POLL_MS = 5000

let sid = ''
let home = ''
let burst = null // last /api/mod answer, null while the dashboard is down
let down = false
let panel = [] // the usage panel's summary rows, as parsed segments
let since = 0 // alerts at or before this were here before the session

export function register(on) {
  on('session.start', async ($, e, next) => {
    sid = await $.session.id()
    home = (await $.env.get('HOME')) || ''
    since = Math.floor((await $.clock.now()) / 1000)
    await refresh($)
    $.clock.every(POLL_MS, async () => {
      await refresh($)
      $.ui.invalidate('ui.render')
    })
    try {
      await $.command.register({ name: 'burst', description: 'Claude Burst and usage panel for this session', immediate: true })
    } catch (err) {
      $.ui.log('could not add /burst: ' + err)
    }
    return next(e)
  })

  on('command.run', { command: 'burst' }, async ($) => {
    await $.ui.open({ id: PANE, title: 'Burst', focus: true, closeOnEscape: true })
    return {}
  })

  on('ui.render', { component: 'AbovePrompt' }, async ($, e, next) => {
    const { Box, Text } = $.ui.resolve(e)
    const theirs = await next(e)
    const row = Box({ flexDirection: 'row', columnGap: 1, children: bandSegments(Text) })
    return theirs ? Box({ flexDirection: 'column', children: [theirs, row] }) : row
  })

  on('ui.render', { component: 'Pane' }, async ($, e, next) => {
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

async function refresh($) {
  try {
    const r = await $.http.fetch(DASHBOARD + '/api/mod?session=' + encodeURIComponent(sid) + '&since=' + since)
    if (!r.ok) throw new Error('status ' + r.status)
    burst = JSON.parse(r.text)
    if (down) $.ui.toast('Burst gateway back')
    down = false
    for (const a of burst.alerts || []) {
      since = Math.max(since, a.ts)
      // Toasts only when the dashboard asks for them: the Ghostty pop-ups
      // show every alert already, and the band shows a standing problem as
      // a line of its own. Info and ok never: they are news, not a problem.
      if (burst.toasts && (a.severity === 'warn' || a.severity === 'error')) $.ui.toast('Burst: ' + a.title)
    }
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

function burstRows() {
  if (down || !burst) return [['Gateway', 'dashboard not answering at ' + DASHBOARD, 'red']]
  const rows = [
    ['Route', burst.route + (burst.overflow && burst.reason ? ' (' + burst.reason + ')' : ''), burst.overflow ? 'yellow' : 'green'],
    ['Anthropic', burst.primary_failing > 0 ? burst.primary_failing + ' failures since the last answer' : 'answering', burst.primary_failing > 0 ? 'red' : 'green'],
  ]
  const s = burst.session
  if (s) {
    rows.push(['Context sent', kTokens(s.context) + (s.compact_at > 0 ? ', compacts at ' + kTokens(s.compact_at) : ', compaction off'), undefined])
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
