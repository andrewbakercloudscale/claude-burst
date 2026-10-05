import { expect, mock, test } from 'claude-code/testing'

const BAND = {
  plugin: 'burst-band',
  component: 'AbovePrompt',
  surface: 'terminal',
  viewport: { columns: 120, rows: 40 },
  props: {},
} as const

const PANE = {
  plugin: 'burst-band',
  component: 'Pane',
  surface: 'terminal',
  requestId: 'burst',
  viewport: { columns: 120, rows: 40 },
  props: { title: 'Burst', isFocused: true, bodyColumns: 80, placement: 'inline', scroll: { offset: 0, bodyRows: 30 }, view: {} },
} as const

const ESC = '\x1b'
const PANEL_FILE =
  '  🤖 Model: Opus 5.5  SID: *4e1a7\n' +
  `  💰 Session: ${ESC}[33m$29.20${ESC}[0m, Burn ${ESC}[32m$0.75/hr${ESC}[0m\n` +
  `  📅 Today: ${ESC}[38;5;196m$6.72${ESC}[0m (by EOD: $17.48)\n` +
  `  🔀 Proxy State: ${ESC}[32mPRIMARY (oauth)${ESC}[0m ${ESC}[1;38;2;0;0;0;48;2;125;249;255m[View]${ESC}[0m\n`

function mod(over: Record<string, unknown> = {}) {
  return {
    version: '0.10.0', route: 'PRIMARY', overflow: false, primary_failing: 0,
    today_usd: 102.25, today_requests: 12963,
    session: { session: 'S1', model: 'claude-opus-5-5', context: 70000, state: 'ok', compact_at: 300000 },
    alerts: [], problems: [], toasts: true, ...over,
  }
}

// Answers everything the mod calls, with the dashboard returning `answer`
// (an object, or null for a dashboard that is down).
// What the Mac outside the mod is, for a test: `claimed` are alert ids a
// panel's pop-up already took, `lines` what the gateway's notice queue holds
// (handed over once), `runs` and `posts` what the mod ran and posted.
type World = { claimed?: string[]; lines?: string[]; runs?: string[][]; posts?: string[] }

function stubs(on, answers: Array<object | null>, toasts: string[], urls: string[] = [], sidebar = false, status: Array<string | undefined> = [], store: Record<string, unknown> = {}, world: World = {}) {
  const clock = mock.clock(on, { now: 1_000_000_000_000 })
  mock.env(on, { HOME: '/Users/me' })
  on('session.start', () => ({ cwd: '/work' }))
  on('session.id', () => ({ value: 'S1' }))
  on('command.register', () => ({ value: undefined }))
  on('ui.render', () => ({ type: 'Text', props: {}, children: ['drawn by Claude Code'] }))
  on('ui.toast', ($, e) => { toasts.push(e.text); return { value: undefined } })
  on('ui.status', ($, e) => { status.push(e.text); return { value: undefined } })
  on('store.get', ($, e) => ({ value: store[e.key] }))
  on('store.set', ($, e) => { store[e.key] = e.value; return { value: undefined } })
  on('fs.read', ($, e) => {
    if (e.path.endsWith('/band/S1.ansi')) return { value: PANEL_FILE }
    if (sidebar && e.path === '/Users/me/.config/claude-panel/mod-installed') return { value: '2026-10-05 11:35:56\n' }
    return { deny: 'no such file' }
  })
  // The claim is a mkdir on the real Mac unless it is answered here.
  on('process.run', ($, e) => {
    world.runs?.push(e.argv)
    const id = e.argv[e.argv.length - 1]
    return { value: { exitCode: (world.claimed || []).includes(id) ? 1 : 0, stdout: '', stderr: '' } }
  })
  let i = 0
  on('http.fetch', ($, e) => {
    if (e.url.endsWith('/api/prompt-notice')) {
      world.posts?.push(e.init.body)
      const lines = world.lines || []
      world.lines = []
      if (lines.length === 0) return { value: { status: 204, ok: true, headers: {}, text: '' } }
      return { value: { status: 200, ok: true, headers: {}, text: JSON.stringify({ systemMessage: lines.join('\n') }) } }
    }
    urls.push(e.url)
    const a = answers[Math.min(i++, answers.length - 1)]
    if (a === null) return { deny: 'connection refused' }
    return { value: { status: 200, ok: true, headers: {}, text: JSON.stringify(a) } }
  })
  return clock
}

async function start($) {
  await $.session.start({ surface: 'terminal', isInteractive: true, cwd: '/work' })
}

test('the band shows the route, the context Burst sends, and the panel rows', async ($, on) => {
  const urls: string[] = []
  stubs(on, [mod()], [], urls)
  await start($)
  expect(urls[0]).toBe('http://127.0.0.1:7788/api/mod?session=S1&since=1000000000')
  const ui = await $.ui.mount(BAND)
  expect(await ui.find({ type: 'Text', text: '⚡ PRIMARY' })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: 'ctx 70k of 300k' })).toBeDefined()
  // The panel's own colours survive: $29.20 was yellow in the panel.
  const cost = await ui.find({ type: 'Text', text: /^\$29\.20$/ })
  expect(cost.props.color).toBe('ansi256(3)')
  expect(await ui.find({ type: 'Text', text: 'Session ' })).toBeDefined()
  expect((await ui.find({ type: 'Text', text: /^\$6\.72$/ })).props.color).toBe('ansi256(196)')
  // Other mods' band content is kept.
  expect(await ui.find({ type: 'Text', text: 'drawn by Claude Code' })).toBeDefined()
})

test('a compaction in progress and an overflow show in the band', async ($, on) => {
  stubs(on, [mod({ route: 'SECONDARY', overflow: true, session: { session: 'S1', context: 310000, state: 'summarising', compact_at: 300000 } })], [])
  await start($)
  const ui = await $.ui.mount(BAND)
  expect(await ui.find({ type: 'Text', text: '⚡ SECONDARY' })).toBeDefined()
  expect((await ui.find({ type: 'Text', text: 'ctx 310k of 300k' })).props.color).toBe('red')
  expect(await ui.find({ type: 'Text', text: 'summarising' })).toBeDefined()
})

test('alerts become toasts once, each claimed so its pop-up stands aside', async ($, on) => {
  const toasts: string[] = []
  const urls: string[] = []
  const world: World = { runs: [] }
  const clock = stubs(on, [
    mod(),
    mod({ alerts: [
      { id: 'a', kind: 'failover', severity: 'warn', title: 'Overflow to the secondary', ts: 1000000100 },
      { id: 'b', kind: 'context', severity: 'info', title: 'Pauseless compaction started', ts: 1000000101 },
    ] }),
    mod(),
  ], toasts, urls, false, [], {}, world)
  await start($)
  await clock.advance(5000)
  expect(toasts).toEqual(['Burst: Overflow to the secondary', 'Burst: Pauseless compaction started'])
  // One claim each, in the panels' own folder, by the event's id.
  const claims = world.runs.filter((r) => r[r.length - 2].endsWith('/alerts-claimed'))
  expect(claims.length).toBe(2)
  expect(claims[0].slice(-2)).toEqual(['/Users/me/.config/claude-panel/alerts-claimed', 'a'])
  await clock.advance(5000)
  // The next poll asks only for what came after the newest alert seen.
  expect(urls[2]).toContain('since=1000000101')
  expect(toasts.length).toBe(2)
})

test('an alert a pop-up already showed is not shown again as a toast', async ($, on) => {
  const toasts: string[] = []
  const clock = stubs(on, [
    mod(),
    mod({ alerts: [
      { id: 'popped', kind: 'network', severity: 'error', title: 'Network offline', ts: 1000000100 },
      { id: 'ours', kind: 'network', severity: 'ok', title: 'Network back', ts: 1000000101 },
    ] }),
  ], toasts, [], false, [], {}, { claimed: ['popped'] })
  await start($)
  await clock.advance(5000)
  expect(toasts).toEqual(['Burst: Network back'])
})

test('compaction lines are taken from the gateway and shown as toasts, once', async ($, on) => {
  const toasts: string[] = []
  const world: World = { lines: ['\u26a1 Burst compaction: 300k context: summarising 885 messages in the background'], posts: [] }
  const clock = stubs(on, [mod()], toasts, [], false, [], {}, world)
  await start($)
  expect(toasts).toEqual(['Burst compaction: 300k context: summarising 885 messages in the background'])
  expect(JSON.parse(world.posts[0])).toEqual({ session_id: 'S1', hook_event_name: 'PostToolUse', mod: true })
  world.lines = ['\u26a1 Burst compaction: done, 62% smaller: 300k \u2192 114k (885 messages summarised)', '\u26a1 Burst compaction: test line from the dashboard']
  await clock.advance(5000)
  await clock.advance(5000)
  expect(toasts.length).toBe(3)
  expect(toasts[2]).toBe('Burst compaction: test line from the dashboard')
})

test('a dashboard that stops answering says so once, and again when it is back', async ($, on) => {
  const toasts: string[] = []
  const clock = stubs(on, [mod(), null, null, mod()], toasts)
  await start($)
  await clock.advance(5000)
  await clock.advance(5000)
  expect(toasts).toEqual(['Burst dashboard not answering'])
  const ui = await $.ui.mount(BAND)
  expect(await ui.find({ type: 'Text', text: '⚡ Burst down' })).toBeDefined()
  await ui.unmount()
  await clock.advance(5000)
  expect(toasts).toEqual(['Burst dashboard not answering', 'Burst gateway back'])
})

test('/burst opens a pane with the panel summary and Burst details', async ($, on) => {
  stubs(on, [mod()], [])
  on('ui.open', () => ({ value: { isPlaced: true } }))
  await start($)
  const answer = await $.command.run({ command: 'burst', args: '' })
  expect(answer).toEqual({})
  const ui = await $.ui.mount(PANE)
  expect(await ui.find({ type: 'Text', text: '  🤖 Model: Opus 5.5  SID: *4e1a7' })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: 'PRIMARY' })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: '70k, compacts at 300k' })).toBeDefined()
  expect(await ui.find({ key: 'dashboard' })).toBeDefined()
  // A background colour's numbers are not read as bold, dim or a colour.
  const view = await ui.find({ type: 'Text', text: /^\[View\]$/ })
  expect(view.props).toEqual({ color: 'rgb(0,0,0)', backgroundColor: 'rgb(125,249,255)', bold: true })
})

test('toasting, the mod marks the session so the panel does not also float its compaction notice', async ($, on) => {
  const world: World = { runs: [] }
  const clock = stubs(on, [mod(), mod(), mod({ toasts: false })], [], [], false, [], {}, world)
  await start($)
  await clock.advance(5000)
  // Marked once, not at every poll.
  const marks = world.runs.filter((r) => r[r.length - 2] === '/Users/me/.config/claude-panel/mod-toasts')
  expect(marks.length).toBe(1)
  expect(marks[0][2]).toContain(': > "$1/$2"')
  expect(marks[0][marks[0].length - 1]).toBe('S1')
  // Toasts turned off: the mark goes, and the notice is the panel's again.
  await clock.advance(5000)
  const last = world.runs[world.runs.length - 1]
  expect(last[2]).toBe('rm -f "$1/$2"')
  expect(last.slice(-2)).toEqual(['/Users/me/.config/claude-panel/mod-toasts', 'S1'])
})

test('toasts are off unless the dashboard turns them on', async ($, on) => {
  const toasts: string[] = []
  const world: World = { lines: ['\u26a1 Burst compaction: summary ready'], runs: [], posts: [] }
  const clock = stubs(on, [mod({ toasts: false }), mod({ toasts: false, alerts: [{ id: 'a', kind: 'network', severity: 'error', title: 'Network offline', ts: 1000000100 }] })], toasts, [], false, [], {}, world)
  await start($)
  await clock.advance(5000)
  expect(toasts).toEqual([])
  // Nothing is claimed and nothing taken: the pop-up and the line under
  // the prompt are left to show them.
  expect(world.runs).toEqual([])
  expect(world.posts).toEqual([])
  expect(world.lines.length).toBe(1)
})

test('a standing problem is a line in the band and in the pane', async ($, on) => {
  stubs(on, [mod({ problems: [{ id: 'a', kind: 'network', severity: 'error', title: 'Network offline', ts: 1 }] })], [])
  on('ui.open', () => ({ value: { isPlaced: true } }))
  await start($)
  const band = await $.ui.mount(BAND)
  expect((await band.find({ type: 'Text', text: '⚠ Network offline' })).props.color).toBe('red')
  await band.unmount()
  await $.command.run({ command: 'burst', args: '' })
  const pane = await $.ui.mount(PANE)
  expect(await pane.find({ type: 'Text', text: 'Network offline' })).toBeDefined()
})

test('the context bar shows what the context is made of, and /context-bar hides it', async ($, on) => {
  const parts = [
    { name: 'System prompt', tokens: 6000 },
    { name: 'System tools', tokens: 14000 },
    { name: 'Messages', tokens: 20000 },
    { name: 'Tool results', tokens: 30000 },
  ]
  stubs(on, [mod({ session: { session: 'S1', context: 70000, state: 'ok', compact_at: 300000, parts } })], [])
  await start($)
  let ui = await $.ui.mount(BAND)
  expect(await ui.find({ type: 'Text', text: '70k of 300k (23%)' })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: 'Tool results 30k' })).toBeDefined()
  const results = await ui.find({ type: 'Text', text: /^█+$/, color: 'green' })
  expect(results).toBeDefined()
  expect(await $.command.run({ command: 'context-bar', args: '' })).toEqual({})
  ui = await $.ui.mount(BAND)
  expect(await ui.find({ type: 'Text', text: 'Tool results 30k' })).toBeUndefined()
  expect(await ui.find({ type: 'Text', text: 'ctx 70k of 300k' })).toBeDefined()
})

test('no context bar before a response reports the parts', async ($, on) => {
  stubs(on, [mod()], [])
  await start($)
  const ui = await $.ui.mount(BAND)
  expect(await ui.find({ type: 'Text', text: /of 300k \(/ })).toBeUndefined()
})

test('with the usage sidebar installed the band stands aside and the status line carries the route and a standing problem', async ($, on) => {
  const status: Array<string | undefined> = []
  const clock = stubs(on, [
    mod({ problems: [{ id: 'a', kind: 'keep-awake', severity: 'warn', title: 'Keep-awake turned off', ts: 1 }] }),
    mod({ problems: [{ id: 'a', kind: 'keep-awake', severity: 'warn', title: 'Keep-awake turned off', ts: 1 }] }),
    mod({ route: 'SECONDARY', overflow: true, session: { session: 'S1', context: 310000, state: 'summarising', compact_at: 300000 } }),
    null,
  ], [], [], true, status)
  await start($)
  expect(status).toEqual(['⚡ PRIMARY · ⚠ Keep-awake turned off'])
  // Nothing of Burst's above the prompt: only what was there already.
  const ui = await $.ui.mount(BAND)
  expect(await ui.find({ type: 'Text', text: 'drawn by Claude Code' })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: '⚡ PRIMARY' })).toBeUndefined()
  expect(await ui.find({ type: 'Text', text: 'Session ' })).toBeUndefined()
  await ui.unmount()
  // Set again only when it changes.
  await clock.advance(5000)
  expect(status.length).toBe(1)
  await clock.advance(5000)
  expect(status[1]).toBe('⚡ SECONDARY · summarising')
  await clock.advance(5000)
  expect(status[2]).toBe('⚡ Burst down')
})

test('without the sidebar the band is drawn and the status line is left alone', async ($, on) => {
  const status: Array<string | undefined> = []
  stubs(on, [mod()], [], [], false, status)
  await start($)
  expect(status).toEqual([])
  const ui = await $.ui.mount(BAND)
  expect(await ui.find({ type: 'Text', text: '⚡ PRIMARY' })).toBeDefined()
})

test('/burst band puts the band back over the sidebar, clears the status line, and is remembered', async ($, on) => {
  const status: Array<string | undefined> = []
  const store: Record<string, unknown> = {}
  stubs(on, [mod()], [], [], true, status, store)
  await start($)
  expect(status).toEqual(['⚡ PRIMARY'])
  expect(await $.command.run({ command: 'burst', args: 'band' })).toEqual({})
  expect(store.band).toBe(true)
  expect(status).toEqual(['⚡ PRIMARY', undefined])
  const ui = await $.ui.mount(BAND)
  expect(await ui.find({ type: 'Text', text: '⚡ PRIMARY' })).toBeDefined()
})

test('a remembered choice of the band wins over the sidebar being there', async ($, on) => {
  const status: Array<string | undefined> = []
  stubs(on, [mod()], [], [], true, status, { band: true })
  await start($)
  expect(status).toEqual([])
  const ui = await $.ui.mount(BAND)
  expect(await ui.find({ type: 'Text', text: '⚡ PRIMARY' })).toBeDefined()
})
