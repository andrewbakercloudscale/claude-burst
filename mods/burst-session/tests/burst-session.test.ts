import { expect, mock, test } from 'claude-code/testing'

const BAND = {
  plugin: 'claude-burst',
  component: 'AbovePrompt',
  surface: 'terminal',
  viewport: { columns: 120, rows: 40 },
  props: {},
} as const

const PANE = {
  plugin: 'claude-burst',
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
// (handed over once), `runs` and `posts` what the mod ran and posted,
// `commands` the slash commands it added, `missing` a program that is not there.
type World = { claimed?: string[]; lines?: string[]; runs?: string[][]; posts?: string[]; commands?: string[]; missing?: string; files?: Record<string, string>; env?: Record<string, string>; pid?: string }

function stubs(on, answers: Array<object | null>, toasts: string[], urls: string[] = [], sidebar = false, status: Array<string | undefined> = [], store: Record<string, unknown> = {}, world: World = {}) {
  const clock = mock.clock(on, { now: 1_000_000_000_000 })
  mock.env(on, { HOME: '/Users/me', ...(world.env || {}) })
  on('session.start', () => ({ cwd: '/work' }))
  on('session.id', () => ({ value: 'S1' }))
  on('command.register', ($, e) => { world.commands?.push(e.name); return { value: undefined } })
  on('ui.render', () => ({ type: 'Text', props: {}, children: ['drawn by Claude Code'] }))
  on('ui.toast', ($, e) => { toasts.push(e.text); return { value: undefined } })
  on('ui.status', ($, e) => { status.push(e.text); return { value: undefined } })
  on('store.get', ($, e) => ({ value: store[e.key] }))
  on('store.set', ($, e) => { store[e.key] = e.value; return { value: undefined } })
  on('fs.read', ($, e) => {
    if (e.path.endsWith('/band/S1.ansi')) return { value: PANEL_FILE }
    if (world.files && e.path in world.files) return { value: world.files[e.path] }
    if (sidebar && e.path === '/Users/me/.config/claude-panel/mod-installed') return { value: '2026-10-05 11:35:56\n' }
    return { deny: 'no such file' }
  })
  // The claim is a mkdir on the real Mac unless it is answered here.
  on('process.run', ($, e) => {
    if (e.argv[0] === 'sh') return { value: { exitCode: 0, stdout: (world.pid || '') + '\n', stderr: '' } }
    world.runs?.push(e.argv)
    if (world.missing && e.argv.includes(world.missing)) return { value: { exitCode: 1, stdout: '', stderr: 'The file ' + world.missing + ' does not exist.' } }
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

test('before the first answer the band is not drawn: unknown is not down', async ($, on) => {
  stubs(on, [mod()], [])
  const early = await $.ui.mount(BAND)
  expect(await early.find({ type: 'Text', text: '⚡ Burst down' })).toBeUndefined()
  await early.unmount()
  await start($)
  const ui = await $.ui.mount(BAND)
  expect(await ui.find({ type: 'Text', text: '⚡ PRIMARY' })).toBeDefined()
  await ui.unmount()
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

test('with the usage sidebar installed the band stands aside, and nothing is put in the status line', async ($, on) => {
  const status: Array<string | undefined> = []
  const clock = stubs(on, [
    mod({ problems: [{ id: 'a', kind: 'keep-awake', severity: 'warn', title: 'Keep-awake turned off', ts: 1 }] }),
    mod({ route: 'SECONDARY', overflow: true, session: { session: 'S1', context: 310000, state: 'summarising', compact_at: 300000 } }),
    null,
  ], [], [], true, status)
  await start($)
  // Nothing of Burst's above the prompt: only what was there already.
  const ui = await $.ui.mount(BAND)
  expect(await ui.find({ type: 'Text', text: 'drawn by Claude Code' })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: '⚡ PRIMARY' })).toBeUndefined()
  expect(await ui.find({ type: 'Text', text: 'Session ' })).toBeUndefined()
  await ui.unmount()
  await clock.advance(5000)
  await clock.advance(5000)
  // Only ever cleared, once: an entry an earlier version pinned comes down.
  expect(status).toEqual([undefined])
})

test('without the sidebar the band is drawn', async ($, on) => {
  stubs(on, [mod()], [], [], false, [])
  await start($)
  const ui = await $.ui.mount(BAND)
  expect(await ui.find({ type: 'Text', text: '⚡ PRIMARY' })).toBeDefined()
})

test('/burst band puts the band back over the sidebar, and is remembered', async ($, on) => {
  const store: Record<string, unknown> = {}
  stubs(on, [mod()], [], [], true, [], store)
  await start($)
  expect(await $.command.run({ command: 'burst', args: 'band' })).toEqual({})
  expect(store.band).toBe(true)
  const ui = await $.ui.mount(BAND)
  expect(await ui.find({ type: 'Text', text: '⚡ PRIMARY' })).toBeDefined()
})

test('a remembered choice of the band wins over the sidebar being there', async ($, on) => {
  const status: Array<string | undefined> = []
  stubs(on, [mod()], [], [], true, status, { band: true })
  await start($)
  expect(status).toEqual([undefined])
  const ui = await $.ui.mount(BAND)
  expect(await ui.find({ type: 'Text', text: '⚡ PRIMARY' })).toBeDefined()
})

test('/claude-burst-revert and /claude-burst-reinstall run the installed scripts in Terminal, with the dashboard down', async ($, on) => {
  const toasts: string[] = []
  const world: World = { runs: [], commands: [] }
  stubs(on, [null], toasts, [], false, [], {}, world)
  await start($)
  expect(world.commands).toContain('claude-burst-revert')
  expect(world.commands).toContain('claude-burst-reinstall')
  expect(await $.command.run({ command: 'claude-burst-revert', args: '' })).toEqual({})
  expect(await $.command.run({ command: 'claude-burst-reinstall', args: '' })).toEqual({})
  expect(world.runs).toEqual([
    ['open', '-a', 'Terminal', '/Users/me/.local/bin/burst-off'],
    ['open', '-a', 'Terminal', '/Users/me/.local/bin/burst-reinstall'],
  ])
  expect(toasts).toContain('Turning Burst off in a Terminal window. Undo: /claude-burst-reinstall')
  expect(toasts).toContain('Reinstalling the newest Burst in a Terminal window')
})

test('a rescue command whose script is not installed says what to run instead', async ($, on) => {
  const toasts: string[] = []
  stubs(on, [mod()], toasts, [], false, [], {}, { missing: '/Users/me/.local/bin/burst-off' })
  await start($)
  await $.command.run({ command: 'claude-burst-revert', args: '' })
  expect(toasts.some((t) => t.startsWith('Could not run /Users/me/.local/bin/burst-off') && t.includes('./install.sh'))).toBe(true)
})

// Hand-off: Claude Code's transcript as a session.compact hook sees it. The
// reply with tool call t2 is two messages here, as Claude Code holds it.
const TRANSCRIPT = [
  { role: 'user', text: 'first task', toolUses: [], handle: 'h0' },
  { role: 'assistant', text: '', toolUses: [{ tool_use_id: 't1', tool: 'Read', input: {} }], handle: 'h1' },
  { role: 'user', text: '', toolUses: [], toolResults: [{ tool_use_id: 't1', text: 'old file' }], handle: 'h2' },
  { role: 'assistant', text: 'done with first', toolUses: [], handle: 'h3' },
  { role: 'user', text: 'second task, the long one', toolUses: [], handle: 'h4' },
  { role: 'assistant', text: 'Looking.', toolUses: [], handle: 'h5' },
  { role: 'assistant', text: '', toolUses: [{ tool_use_id: 't2', tool: 'Bash', input: {} }], handle: 'h6' },
  { role: 'user', text: '', toolUses: [], toolResults: [{ tool_use_id: 't2', text: 'output' }], handle: 'h7' },
  { role: 'assistant', text: 'done with second', toolUses: [], handle: 'h8' },
]
const LEAD = '<system-reminder>\nThe earlier part of this conversation was compacted by claude-burst to save context. Summary of it:\n<summary>\nTHE GIST\n</summary>\n</system-reminder>'
const HANDOFF_FILE = '/Users/me/.config/claude-burst/handoff/S1.json'
const HOSTS_IN = '127.0.0.1 localhost\n# BEGIN claude-burst hosts\n127.0.0.1 api.anthropic.com\n# END claude-burst hosts\n'
const HOSTS_OUT = '127.0.0.1 localhost\n# 127.0.0.1 api.anthropic.com\n'

function handoff(over: Record<string, unknown> = {}) {
  return JSON.stringify({
    session: 'S1', lead: LEAD, of: 'abc', messages: 5, raw: 994000,
    last: { role: 'user', text: 'secondtask,thelongone' }, first: { role: 'assistant', tool: 't2' }, ...over,
  })
}

// Claude Code's own compaction, counted: a hand-off must not reach it.
function core(on, calls: string[]) {
  on('session.compact', ($, e) => { calls.push(e.trigger); return { messages: [{ role: 'user', text: 'CORE SUMMARY', toolUses: [] }] } })
  on('command.run', { command: 'compact' }, () => { calls.push('/compact'); return {} })
  on('ui.log', ($, e) => { calls.push('log: ' + e.text); return { value: undefined } })
}

test("Claude Code's compaction is answered with Burst's summary: no summary request, the kept messages whole", async ($, on) => {
  const toasts: string[] = []
  const calls: string[] = []
  stubs(on, [mod()], toasts, [], false, [], {}, { files: { [HANDOFF_FILE]: handoff() } })
  core(on, calls)
  await start($)
  const r = await $.session.compact({ trigger: 'auto', messages: TRANSCRIPT })
  expect(calls).toEqual([])
  // The cut is the start of the reply, not of its tool call: "Looking." is kept.
  expect(r.messages.map((m) => m.handle)).toEqual([undefined, 'h5', 'h6', 'h7', 'h8'])
  expect(r.messages[0]).toEqual({ role: 'user', text: LEAD, toolUses: [] })
  expect(toasts).toContain("Burst's summary handed to Claude Code: 5 messages replaced, no summary request")
})

test('a hand-off that does not fit the transcript, or is turned off, leaves the compaction to Claude Code', async ($, on) => {
  const calls: string[] = []
  const world: World = { files: { [HANDOFF_FILE]: handoff({ first: { role: 'assistant', tool: 'gone' }, last: { role: 'user', text: 'nothingsaidlikethisanywhere' } }) } }
  stubs(on, [mod()], [], [], false, [], {}, world)
  core(on, calls)
  await start($)
  expect((await $.session.compact({ trigger: 'auto', messages: TRANSCRIPT })).messages[0].text).toBe('CORE SUMMARY')
  world.files = { [HANDOFF_FILE]: handoff(), '/Users/me/.config/claude-burst/mod.json': '{"toasts":true,"handoff":false}' }
  expect((await $.session.compact({ trigger: 'auto', messages: TRANSCRIPT })).messages[0].text).toBe('CORE SUMMARY')
  // No file at all, a subagent's transcript, and /compact with instructions.
  world.files = {}
  await $.session.compact({ trigger: 'manual', messages: TRANSCRIPT })
  world.files = { [HANDOFF_FILE]: handoff() }
  await $.session.compact({ trigger: 'auto', agentId: 'a1', messages: TRANSCRIPT })
  await $.session.compact({ trigger: 'manual', instructions: 'keep the plan', messages: TRANSCRIPT })
  // A kept part that would open with tool results is refused.
  world.files = { [HANDOFF_FILE]: handoff({ first: { role: 'user', tool: 't2' }, last: { role: 'assistant', tool: 't2' } }) }
  await $.session.compact({ trigger: 'auto', messages: TRANSCRIPT })
  expect(calls).toEqual(['auto', 'auto', 'manual', 'auto', 'manual', 'auto'])
})

test('a prompt named by its text is found only after the message the summary ends on, and a summary already handed over is not handed again', async ($, on) => {
  const calls: string[] = []
  const world: World = { files: { [HANDOFF_FILE]: handoff({ first: { role: 'user', text: 'secondtask,thelongone' }, last: { role: 'assistant', text: 'donewithfirst' } }) } }
  stubs(on, [mod()], [], [], false, [], {}, world)
  core(on, calls)
  await start($)
  // The same words said earlier are not the cut.
  const twice = [{ role: 'user', text: 'second task, the long one', toolUses: [], handle: 'e0' }, { role: 'assistant', text: 'which one?', toolUses: [], handle: 'e1' }, ...TRANSCRIPT]
  const r = await $.session.compact({ trigger: 'manual', messages: twice })
  expect(r.messages.map((m) => m.handle)).toEqual([undefined, 'h4', 'h5', 'h6', 'h7', 'h8'])
  expect((await $.session.compact({ trigger: 'manual', messages: r.messages })).messages[0].text).toBe('CORE SUMMARY')
  expect(calls).toEqual(['manual'])
})

test('a prompt too short to name is found by the reply before it, and only where it says the same', async ($, on) => {
  const calls: string[] = []
  const world: World = { files: { [HANDOFF_FILE]: handoff({ first: { role: 'user', text: 'doboth' }, last: { role: 'assistant', text: 'whichofthetwodoyouwant?' } }) } }
  stubs(on, [mod()], [], [], false, [], {}, world)
  core(on, calls)
  await start($)
  const short = [
    ...TRANSCRIPT,
    { role: 'assistant', text: 'which of the two do you want?', toolUses: [], handle: 's0' },
    { role: 'user', text: 'do both', toolUses: [], handle: 's1' },
    { role: 'assistant', text: 'doing both', toolUses: [], handle: 's2' },
  ]
  const r = await $.session.compact({ trigger: 'auto', messages: short })
  expect(r.messages.map((m) => m.handle)).toEqual([undefined, 's1', 's2'])
  // The same reply followed by other words is not the cut.
  const other = short.map((m) => (m.handle === 's1' ? { ...m, text: 'neither' } : m))
  expect((await $.session.compact({ trigger: 'auto', messages: other })).messages[0].text).toBe('CORE SUMMARY')
  expect(calls).toEqual(['auto'])
})

test('a session that has left Burst is compacted with the summary as soon as it is seen, once', async ($, on) => {
  const toasts: string[] = []
  const calls: string[] = []
  const world: World = { files: { [HANDOFF_FILE]: handoff(), '/etc/hosts': HOSTS_OUT } }
  const clock = stubs(on, [null], toasts, [], false, [], {}, world)
  core(on, calls)
  await start($)
  await clock.advance(5000)
  await clock.advance(5000)
  expect(calls).toEqual(['/compact'])
  // Claude Code queued that /compact behind a turn and it comes later. The
  // transcript does not fit: nothing is compacted, Claude Code writes no summary.
  expect(String((await $.session.compact({ trigger: 'manual', messages: TRANSCRIPT.slice(0, 2) })).skip)).toContain("Burst's summary does not fit")
  expect(calls).toEqual(['/compact'])
  // Typed again, it is the user's: Claude Code writes its own.
  expect((await $.session.compact({ trigger: 'manual', messages: TRANSCRIPT.slice(0, 2) })).messages[0].text).toBe('CORE SUMMARY')
})

test('asked for by the mod, a summary that does not fit compacts nothing', async ($, on) => {
  const calls: string[] = []
  const world: World = { files: { [HANDOFF_FILE]: handoff(), '/etc/hosts': HOSTS_OUT } }
  const clock = stubs(on, [null], [], [], false, [], {}, world)
  on('session.compact', ($, e) => { calls.push(e.trigger); return { messages: [] } })
  // /compact is held open, as Claude Code holds it while it compacts.
  let release = () => {}
  let held = false
  on('command.run', { command: 'compact' }, () => new Promise((r) => { held = true; release = () => r({}) }))
  await start($)
  const tick = clock.advance(5000)
  for (let i = 0; i < 50 && !held; i++) await new Promise((r) => setTimeout(r, 5))
  expect(held).toBe(true)
  const answer = await $.session.compact({ trigger: 'manual', messages: TRANSCRIPT.slice(0, 3) })
  release()
  await tick
  expect(String(answer.skip)).toContain("Burst's summary does not fit")
  expect(calls).toEqual([])
})

test("/compact-async-full hands Burst's summary over with Burst in the path, and never has Claude Code write one", async ($, on) => {
  const toasts: string[] = []
  const calls: string[] = []
  const world: World = { files: { [HANDOFF_FILE]: handoff({ raw: 90000 }), '/etc/hosts': HOSTS_IN } }
  stubs(on, [null], toasts, [], false, [], {}, world)
  core(on, calls)
  await start($)
  expect(await $.command.run({ command: 'compact-async-full', args: '' })).toEqual({})
  await new Promise((r) => setTimeout(r, 0))
  expect(calls).toEqual(['/compact'])
  // The /compact it ran comes with a transcript the summary does not fit: skipped, not Claude Code's.
  expect(String((await $.session.compact({ trigger: 'manual', messages: TRANSCRIPT.slice(0, 2) })).skip)).toContain("Burst's summary does not fit")
  // With one it fits, the summary is handed over.
  await $.command.run({ command: 'compact-async-full', args: '' })
  await new Promise((r) => setTimeout(r, 0))
  expect((await $.session.compact({ trigger: 'manual', messages: TRANSCRIPT })).messages[0].text).toBe(LEAD)
  expect(calls).toEqual(['/compact', '/compact'])
  // No summary held: it says so and runs nothing.
  world.files = { '/etc/hosts': HOSTS_IN }
  await $.command.run({ command: 'compact-async-full', args: '' })
  await new Promise((r) => setTimeout(r, 0))
  expect(calls).toEqual(['/compact', '/compact'])
  expect(toasts.some((t) => t.includes('Burst holds no summary of this session'))).toBe(true)
})

test('the size the gateway last reported counts once the gateway is gone', async ($, on) => {
  const calls: string[] = []
  // The file was written when the session was small; the gateway said 600k before it went.
  const world: World = { files: { [HANDOFF_FILE]: handoff({ raw: 0 }), '/etc/hosts': HOSTS_OUT } }
  const clock = stubs(on, [mod({ session: { session: 'S1', context: 135000, state: 'compacted', compact_at: 300000, raw: 600000 } }), null], [], [], false, [], {}, world)
  core(on, calls)
  await start($)
  await clock.advance(5000)
  expect(calls).toEqual(['/compact'])
})

test('no hand-off while Burst is in the path, the history is short, or nothing says where requests go', async ($, on) => {
  const calls: string[] = []
  const world: World = { files: { [HANDOFF_FILE]: handoff({ raw: 600000 }), '/etc/hosts': HOSTS_IN } }
  // The gateway is down but still in the path: requests fail, they are not sent whole.
  const clock = stubs(on, [null], [], [], false, [], {}, world)
  core(on, calls)
  await start($)
  await clock.advance(5000)
  world.files = { [HANDOFF_FILE]: handoff({ raw: 90000 }), '/etc/hosts': HOSTS_OUT }
  await clock.advance(5000)
  world.files = { [HANDOFF_FILE]: handoff({ raw: 600000 }) }
  await clock.advance(5000)
  world.files = { '/etc/hosts': HOSTS_OUT }
  await clock.advance(5000)
  expect(calls).toEqual([])
})

const MOD_FILE = '/Users/me/.config/claude-burst/mod.json'

test('a history near the 1M window is compacted with Burst in the path only where the dashboard turns that on, once', async ($, on) => {
  const calls: string[] = []
  // Off by default: it has costs with Burst working.
  const world: World = { files: { [HANDOFF_FILE]: handoff({ raw: 994000 }), '/etc/hosts': HOSTS_IN } }
  const clock = stubs(on, [null], [], [], false, [], {}, world)
  core(on, calls)
  await start($)
  await clock.advance(5000)
  expect(calls).toEqual([])
  const inPath = JSON.stringify({ handoff_in_path: true })
  world.files = { [HANDOFF_FILE]: handoff({ raw: 799999 }), '/etc/hosts': HOSTS_IN, [MOD_FILE]: inPath }
  await clock.advance(5000)
  expect(calls).toEqual([])
  world.files = { [HANDOFF_FILE]: handoff({ raw: 800000 }), '/etc/hosts': HOSTS_IN, [MOD_FILE]: inPath }
  await clock.advance(5000)
  await clock.advance(5000)
  expect(calls).toEqual(['/compact'])
})

test('a session opened again is compacted with the summary as it opens, in the path, and a reload of the mod is not a restart', async ($, on) => {
  const calls: string[] = []
  const store: Record<string, unknown> = {}
  // 600k: under what the in-path option needs, and that option is off.
  const world: World = { files: { [HANDOFF_FILE]: handoff({ raw: 600000 }), '/etc/hosts': HOSTS_IN }, pid: '4242' }
  const clock = stubs(on, [null], [], [], false, [], store, world)
  core(on, calls)
  await start($)
  await clock.advance(5000)
  await clock.advance(5000)
  expect(calls).toEqual(['/compact'])
  // The mod reloads in the same process, with a newer summary waiting.
  world.files = { [HANDOFF_FILE]: handoff({ raw: 600000, of: 'def' }), '/etc/hosts': HOSTS_IN }
  await start($)
  await clock.advance(5000)
  expect(calls).toEqual(['/compact'])
  // Another process: opened again.
  world.pid = '5151'
  await start($)
  await clock.advance(5000)
  expect(calls).toEqual(['/compact', '/compact'])
})

test('a session opened again that Burst never summarised, or with the hand-off off, is left alone', async ($, on) => {
  const calls: string[] = []
  const world: World = { files: { '/etc/hosts': HOSTS_IN }, pid: '4242' }
  const clock = stubs(on, [null], [], [], false, [], {}, world)
  core(on, calls)
  await start($)
  // A summary written later in this process is not a restart's.
  world.files = { [HANDOFF_FILE]: handoff({ raw: 600000 }), '/etc/hosts': HOSTS_IN }
  await clock.advance(5000)
  world.pid = '5151'
  world.files = { [HANDOFF_FILE]: handoff({ raw: 600000 }), '/etc/hosts': HOSTS_IN, [MOD_FILE]: '{"handoff":false}' }
  await start($)
  await clock.advance(5000)
  expect(calls).toEqual([])
})

test('a session pointed at the gateway by ANTHROPIC_BASE_URL is in the path whatever /etc/hosts says', async ($, on) => {
  const calls: string[] = []
  const clock = stubs(on, [null], [], [], false, [], {}, { files: { [HANDOFF_FILE]: handoff({ raw: 600000 }), '/etc/hosts': HOSTS_OUT }, env: { ANTHROPIC_BASE_URL: 'http://127.0.0.1:7777' } })
  core(on, calls)
  await start($)
  await clock.advance(5000)
  expect(calls).toEqual([])
})

test('base-url mode set in Claude Code\'s settings is in the path too', async ($, on) => {
  const calls: string[] = []
  const clock = stubs(on, [null], [], [], false, [], {}, { files: { [HANDOFF_FILE]: handoff({ raw: 600000 }), '/etc/hosts': HOSTS_OUT, '/Users/me/.claude/settings.json': '{"env":{"ANTHROPIC_BASE_URL":"http://localhost:7777"}}' } })
  core(on, calls)
  await start($)
  await clock.advance(5000)
  expect(calls).toEqual([])
})
