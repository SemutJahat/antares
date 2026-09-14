import { describe, expect, test } from 'bun:test'
import {
  applyStreamEvent,
  createForegroundRun,
  createFrameBatcher,
  createStandingAttach,
} from './chatStreamController.ts'

/* ---------- helpers ---------- */

/**
 * Minimal harness recording every callback the controller invokes. Tests read
 * consumer-observable outcomes (deltas, patches, live snapshot, ask/usage/
 * title/session), not private controller state.
 */
function harness(overrides = {}) {
  const state = {
    live: { turn: 1 },
    patches: [],
    deltas: [],
    session: [],
    askId: undefined,
    usage: undefined,
    title: undefined,
  }
  return {
    patchAssistant: (fn) => state.patches.push(fn),
    appendDelta: (segment, delta) => state.deltas.push({ segment, delta }),
    setLive: (updater) => {
      state.live = updater(state.live)
    },
    setAskId: (id) => {
      state.askId = id
    },
    setUsage: (u) => {
      state.usage = u
    },
    setTitle: (title) => {
      state.title = title
    },
    onSession: (id, title) => state.session.push({ id, title }),
    showReasoning: () => true,
    errorMessage: () => 'something went wrong',
    __state: state,
    ...overrides,
  }
}

/** Reduce recorded patches into a synthetic assistant message. */
function reduce(initial, patches) {
  return patches.reduce((m, fn) => fn(m), initial)
}

/** Manual raf: nothing runs until the test explicitly calls flush(). */
function manualSchedule() {
  const pending = new Map()
  let next = 1
  return {
    schedule: {
      schedule(fn) {
        const h = next++
        pending.set(h, fn)
        return h
      },
      cancel(h) {
        pending.delete(h)
      },
    },
    flush() {
      const fns = [...pending.values()]
      pending.clear()
      for (const fn of fns) fn()
    },
    pendingCount: () => pending.size,
  }
}

function manualClock() {
  let next = 1
  const timers = new Map()
  return {
    setTimer(fn, ms) {
      const h = next++
      timers.set(h, { fn, ms })
      return h
    },
    clearTimer(h) {
      timers.delete(h)
    },
    fireOne() {
      const [h, entry] = [...timers.entries()][0] ?? []
      if (!entry) return false
      timers.delete(h)
      entry.fn()
      return true
    },
    pendingCount: () => timers.size,
  }
}

/** Scripted transport: hand-fed frames and errors, records connect/close. */
function scriptedTransport() {
  const state = { connections: 0, closed: 0, onEvent: null, onError: null }
  const transport = (_path, onEvent, onError) => {
    state.connections++
    state.onEvent = onEvent
    state.onError = onError
    return () => {
      state.closed++
    }
  }
  return {
    transport,
    state,
    emit: (e) => state.onEvent?.(e),
    fail: (e) => state.onError?.(e),
  }
}

/* ---------- applyStreamEvent (state reduction) ---------- */

describe('applyStreamEvent', () => {
  test('text/reasoning route through appendDelta and honour showReasoning', () => {
    const h = harness()
    applyStreamEvent({ type: 'text', delta: 'hi' }, h)
    applyStreamEvent({ type: 'reasoning', delta: 'think' }, h)
    expect(h.__state.deltas).toEqual([
      { segment: 'text', delta: 'hi' },
      { segment: 'reasoning', delta: 'think' },
    ])

    const suppressed = harness({ showReasoning: () => false })
    applyStreamEvent({ type: 'reasoning', delta: 'secret' }, suppressed)
    expect(suppressed.__state.deltas).toEqual([])
  })

  test('tool_call increments turn; tool_result records isError; reset wipes assistant', () => {
    const h = harness()
    applyStreamEvent({ type: 'tool_call', id: 't1', name: 'read', arguments: '{}' }, h)
    expect(h.__state.live).toEqual({ turn: 2, tool: 'read', notice: undefined })

    applyStreamEvent(
      { type: 'tool_result', id: 't1', content: 'nope', is_error: true },
      h,
    )
    const msg = reduce({ id: 'a', role: 'assistant', content: '' }, h.__state.patches)
    expect(msg.toolCalls?.[0]).toMatchObject({
      id: 't1',
      name: 'read',
      result: 'nope',
      isError: true,
      running: false,
    })

    applyStreamEvent({ type: 'reset' }, h)
    const afterReset = reduce(
      { id: 'a', role: 'assistant', content: 'partial' },
      h.__state.patches,
    )
    expect(afterReset).toMatchObject({
      content: '',
      reasoning: undefined,
      toolCalls: undefined,
      segments: [],
      error: undefined,
    })
  })

  test('ask records id and pauses live; error stamps assistant, defaulting to fallback', () => {
    const h = harness()
    applyStreamEvent({ type: 'ask', id: 'ask_1' }, h)
    expect(h.__state.askId).toBe('ask_1')
    expect(h.__state.live).toMatchObject({ waiting: true, tool: undefined })

    applyStreamEvent({ type: 'error', error: 'boom' }, h)
    const msg = reduce({ id: 'a', role: 'assistant', content: '' }, h.__state.patches)
    // The reducer normalises every terminal error into the single-key JSON
    // block the transcript renders and Copy hands to the clipboard. A plain
    // 'boom' from the server becomes `{"error":"boom"}` verbatim on screen.
    expect(msg.error).toBe(JSON.stringify({ error: 'boom' }, null, 2))
    expect(msg.errorSource).toBe('server')

    const h2 = harness()
    applyStreamEvent({ type: 'error' }, h2)
    const msg2 = reduce({ id: 'a', role: 'assistant', content: '' }, h2.__state.patches)
    // Fallback message is threaded through the same normaliser: identical shape.
    expect(msg2.error).toBe(JSON.stringify({ error: 'something went wrong' }, null, 2))
  })

  test('usage sets only the fields the event carries', () => {
    const h = harness()
    applyStreamEvent(
      { type: 'usage', context_tokens: 512, context_window: 8000, input_tokens: 100 },
      h,
    )
    expect(h.__state.usage).toEqual({ used: 512, window: 8000 })

    const h2 = harness()
    applyStreamEvent({ type: 'usage', input_tokens: 100 }, h2)
    expect(h2.__state.usage).toBeUndefined()
  })
})

/* ---------- frame batcher ---------- */

describe('frame batcher', () => {
  test('caps live reasoning at the configured window on flush', () => {
    const sched = manualSchedule()
    let messages = [{ id: 'a', role: 'assistant', content: '' }]
    const batcher = createFrameBatcher({
      schedule: sched.schedule,
      setMessages: (updater) => {
        messages = updater(messages)
      },
      maxLiveReasoningChars: () => 10,
    })
    for (let i = 0; i < 50; i++) batcher.enqueueDelta('a', 'reasoning', 'x')
    // Adjacent deltas coalesce into one queued patch — a 50-token burst schedules
    // exactly one frame, not fifty.
    expect(sched.pendingCount()).toBe(1)
    sched.flush()
    expect(messages[0].reasoning?.length).toBe(10)
  })

  test('drain runs synchronously and cancels the pending frame', () => {
    const sched = manualSchedule()
    let commits = 0
    const batcher = createFrameBatcher({
      schedule: sched.schedule,
      setMessages: () => {
        commits++
      },
      maxLiveReasoningChars: () => 48000,
    })
    batcher.enqueuePatch('a', (m) => m)
    batcher.drain()
    expect(commits).toBe(1)
    expect(sched.pendingCount()).toBe(0)
    // A second drain with nothing queued is a no-op — no ghost commit.
    batcher.drain()
    expect(commits).toBe(1)
  })
})

/* ---------- standing attach harness ---------- */

function makeAttach({ transport, timers, sched, overrides = {} }) {
  const state = {
    messages: [],
    streaming: false,
    live: { turn: 1 },
    title: '',
    done: 0,
    doneHadLive: false,
    hydrated: 0,
    hydrateDetail: null,
    events: [],
    authFailure: 0,
    foregroundActive: false,
    askId: undefined,
    usage: undefined,
  }
  const batcher = createFrameBatcher({
    schedule: sched.schedule,
    setMessages: (updater) => {
      state.messages = updater(state.messages)
    },
    maxLiveReasoningChars: () => 48000,
  })
  const applyToAssistant = (id, event) =>
    applyStreamEvent(event, {
      patchAssistant: (fn) => batcher.enqueuePatch(id, fn),
      appendDelta: (seg, delta) => batcher.enqueueDelta(id, seg, delta),
      setLive: (updater) => {
        state.live = updater(state.live)
      },
      setAskId: (v) => {
        state.askId = v
      },
      setUsage: (u) => {
        state.usage = u
      },
      setTitle: (title) => {
        state.title = title
      },
      showReasoning: () => true,
      errorMessage: () => 'oops',
    })
  const standing = createStandingAttach('sid_1', {
    isForegroundActive: () => state.foregroundActive,
    ensureAssistant: () => {
      const id = `live_${state.messages.length}`
      state.messages = [...state.messages, { id, role: 'assistant', content: '' }]
      state.streaming = true
      return id
    },
    onDone: (hadLive) => {
      batcher.drain()
      state.streaming = false
      state.done++
      state.doneHadLive = hadLive
    },
    onSessionHydrated: (detail) => {
      state.hydrated++
      state.hydrateDetail = detail
    },
    onAuthFailure: () => {
      state.streaming = false
      state.authFailure++
    },
    onEvent: (id, event) => {
      state.events.push({ id, event })
      applyToAssistant(id, event)
    },
    fetchSession:
      overrides.fetchSession ??
      (() =>
        Promise.resolve({
          session: { id: 'sid_1', title: 'From server', model: '', provider: '' },
          messages: [],
        })),
    transport: transport.transport,
    setTimer: timers.setTimer,
    clearTimer: timers.clearTimer,
    isAuthError: overrides.isAuthError ?? (() => false),
    attachRetryMs: 3000,
    attachIdleMs: 1500,
  })
  return { state, standing, sched, batcher }
}

async function settle() {
  await Promise.resolve()
  await Promise.resolve()
  await Promise.resolve()
}

/* ---------- standing attach behaviour ---------- */

describe('standing attach', () => {
  test('dedups replayed cursors across a broken pipe', () => {
    const sched = manualSchedule()
    const timers = manualClock()
    const transport = scriptedTransport()
    const h = makeAttach({ transport, timers, sched })

    transport.emit({ type: 'text', delta: 'Hel', cursor: 1 })
    transport.emit({ type: 'text', delta: 'lo', cursor: 2 })
    // Simulate a broken pipe replay: cursors 1 and 2 arrive a second time.
    transport.emit({ type: 'text', delta: 'Hel', cursor: 1 })
    transport.emit({ type: 'text', delta: 'lo', cursor: 2 })
    transport.emit({ type: 'text', delta: '!', cursor: 3 })
    sched.flush()

    expect(h.state.messages[0].content).toBe('Hello!')
    expect(h.state.events.map((e) => e.event.cursor)).toEqual([1, 2, 3])
  })

  test('done drains, hydrates once per idle period, schedules next connect with cursor 0', async () => {
    const sched = manualSchedule()
    const timers = manualClock()
    const transport = scriptedTransport()
    const captured = []
    const original = transport.transport
    // Wrap to capture the URL each connection uses.
    const wrapped = (path, onEvent, onError) => {
      captured.push(path)
      return original(path, onEvent, onError)
    }
    transport.transport = wrapped

    const h = makeAttach({ transport, timers, sched })

    transport.emit({ type: 'text', delta: 'hi', cursor: 1 })
    transport.emit({ type: 'done', cursor: 2 })
    sched.flush()
    await settle()

    expect(h.state.done).toBe(1)
    expect(h.state.doneHadLive).toBe(true)
    expect(h.state.hydrated).toBe(1)
    expect(timers.pendingCount()).toBe(1)

    // Fire the idle timer → a fresh connection is opened with cursor reset.
    timers.fireOne()
    expect(transport.state.connections).toBe(2)
    expect(captured[0]).toContain('cursor=0')
    expect(captured[1]).toContain('cursor=0')
  })

  test('idle done (no events) hydrates exactly once — later idle polls do not re-download', async () => {
    const sched = manualSchedule()
    const timers = manualClock()
    const transport = scriptedTransport()
    const h = makeAttach({ transport, timers, sched })

    transport.emit({ type: 'done', cursor: 0 })
    await settle()
    expect(h.state.hydrated).toBe(1)
    timers.fireOne()

    transport.emit({ type: 'done', cursor: 0 })
    await settle()
    expect(h.state.hydrated).toBe(1)
  })

  test('done + hydrate race: hydration completes before the next connect fires', async () => {
    const sched = manualSchedule()
    const timers = manualClock()
    const transport = scriptedTransport()
    let resolveHydrate
    const h = makeAttach({
      transport,
      timers,
      sched,
      overrides: {
        fetchSession: () =>
          new Promise((res) => {
            resolveHydrate = res
          }),
      },
    })

    transport.emit({ type: 'text', delta: 'hi', cursor: 1 })
    transport.emit({ type: 'done', cursor: 2 })
    // While hydrate is unresolved, the next connect timer must NOT be scheduled
    // yet — a stale hydrate response would otherwise overwrite live deltas.
    await settle()
    expect(timers.pendingCount()).toBe(0)

    resolveHydrate({
      session: { id: 'sid_1', title: 'Ready', model: '', provider: '' },
      messages: [],
    })
    await settle()
    expect(h.state.hydrated).toBe(1)
    expect(timers.pendingCount()).toBe(1)
  })

  test('cancellation clears the retry timer and aborts the connection', () => {
    const sched = manualSchedule()
    const timers = manualClock()
    const transport = scriptedTransport()
    const h = makeAttach({ transport, timers, sched })

    transport.fail(new Error('boom'))
    expect(timers.pendingCount()).toBe(1)
    h.standing.dispose()
    expect(timers.pendingCount()).toBe(0)
    expect(transport.state.closed).toBeGreaterThanOrEqual(1)
  })

  test('late event after dispose (navigation) does not mutate state', () => {
    const sched = manualSchedule()
    const timers = manualClock()
    const transport = scriptedTransport()
    const h = makeAttach({ transport, timers, sched })

    h.standing.dispose()
    transport.emit({ type: 'text', delta: 'ghost', cursor: 1 })
    sched.flush()
    // No assistant was ever ensured — the disposed session refused the late
    // event that arrived after the user navigated away.
    expect(h.state.messages).toEqual([])
    expect(h.state.events).toEqual([])
  })

  test('late event after switching sessions never mutates the new transcript', async () => {
    // First session: dispose after receiving an event, then a stale frame
    // arrives (server replay a moment after we cancelled).
    const sched = manualSchedule()
    const timers = manualClock()
    const first = scriptedTransport()
    const h1 = makeAttach({ transport: first, timers, sched })
    first.emit({ type: 'text', delta: 'old', cursor: 1 })
    sched.flush()
    expect(h1.state.messages[0].content).toBe('old')

    h1.standing.dispose()

    // Second session: a completely fresh attach with its own transport, state,
    // and cursor. A stale frame targeted at the first must not touch it.
    const second = scriptedTransport()
    const h2 = makeAttach({ transport: second, timers, sched })
    first.emit({ type: 'text', delta: 'STALE', cursor: 999 })
    second.emit({ type: 'text', delta: 'new', cursor: 1 })
    sched.flush()
    expect(h2.state.messages[0].content).toBe('new')
    // The disposed first-session state received nothing.
    expect(h1.state.messages[0].content).toBe('old')
    await settle()
  })

  test('foreground turn defers the standing attach until it clears', async () => {
    const sched = manualSchedule()
    const timers = manualClock()
    const transport = scriptedTransport()
    const h = makeAttach({ transport, timers, sched })

    transport.emit({ type: 'done', cursor: 0 })
    await settle()
    expect(timers.pendingCount()).toBe(1)

    h.state.foregroundActive = true
    timers.fireOne() // connect() runs, sees foreground, defers
    expect(transport.state.connections).toBe(1)
    expect(timers.pendingCount()).toBe(1)

    h.state.foregroundActive = false
    timers.fireOne()
    expect(transport.state.connections).toBe(2)
  })

  test('auth failure stops the retry loop', () => {
    const sched = manualSchedule()
    const timers = manualClock()
    const transport = scriptedTransport()
    const h = makeAttach({
      transport,
      timers,
      sched,
      overrides: { isAuthError: () => true },
    })

    transport.fail({ status: 401 })
    expect(h.state.authFailure).toBe(1)
    // No retry queued — a 401 does not fix itself and would otherwise flood the
    // daemon log every three seconds.
    expect(timers.pendingCount()).toBe(0)
  })

  test('tool error flows through a live tool_result onto the reduced message', () => {
    const sched = manualSchedule()
    const timers = manualClock()
    const transport = scriptedTransport()
    const h = makeAttach({ transport, timers, sched })

    transport.emit({
      type: 'tool_call',
      id: 't1',
      name: 'read',
      arguments: '{}',
      cursor: 1,
    })
    transport.emit({
      type: 'tool_result',
      id: 't1',
      content: 'permission denied',
      is_error: true,
      cursor: 2,
    })
    sched.flush()

    const msg = h.state.messages[0]
    expect(msg.toolCalls?.[0]).toMatchObject({
      id: 't1',
      result: 'permission denied',
      isError: true,
      running: false,
    })
  })

  test('reasoning burst coalesces into one commit and respects the batcher cap', () => {
    const sched = manualSchedule()
    const timers = manualClock()
    const transport = scriptedTransport()
    const h = makeAttach({ transport, timers, sched })

    for (let i = 0; i < 500; i++) {
      transport.emit({ type: 'reasoning', delta: 'x', cursor: 100 + i })
    }
    // Assistant was ensured immediately, but no reasoning has landed yet: the
    // 500 deltas sit in the batcher queue until the frame flushes.
    expect(h.state.messages).toHaveLength(1)
    expect(h.state.messages[0].reasoning ?? '').toBe('')
    sched.flush()
    expect(h.state.messages).toHaveLength(1)
    expect(h.state.messages[0].reasoning?.length).toBe(500)
  })
})

/* ---------- foreground run ---------- */

/** Scripted /chat POST transport: records each connect + close and hands the
 *  test back the event/error/done callbacks so it can script a full turn. */
function scriptedPostTransport() {
  const state = { connections: 0, closed: 0, onEvent: null, onError: null, onDone: null, body: null, path: null }
  const transport = (path, body, onEvent, onError, onDone) => {
    state.connections++
    state.onEvent = onEvent
    state.onError = onError
    state.onDone = onDone
    state.body = body
    state.path = path
    return () => {
      state.closed++
    }
  }
  return {
    transport,
    state,
    emit: (e) => state.onEvent?.(e),
    fail: (e) => state.onError?.(e),
    finish: () => state.onDone?.(),
  }
}

function makeForeground({ transport, overrides = {} }) {
  const state = {
    events: [],
    doneCalls: 0,
    errorCalls: 0,
    finalCalls: 0,
    cleanEofCalls: 0,
    hydrated: null,
    drained: 0,
    session: null,
    currentSid: 'currentSid' in overrides ? overrides.currentSid : 'sid_A',
  }
  const run = createForegroundRun({
    assistantId: 'assist_1',
    path: '/chat',
    body: { hello: 1 },
    currentSessionId: () => state.currentSid,
    onSession: (id, title) => {
      state.session = { id, title }
      if (overrides.adoptSession) state.currentSid = id
    },
    onEvent: (event) => state.events.push(event),
    onDone: () => {
      state.doneCalls++
    },
    onError: (err) => {
      state.errorCalls++
      state.errorMsg = err?.message
    },
    onFinal: () => {
      state.finalCalls++
    },
    onCleanEof: () => {
      state.cleanEofCalls++
    },
    onSessionHydrated: (detail) => {
      state.hydrated = detail
    },
    fetchSession:
      overrides.fetchSession ??
      ((sid) =>
        Promise.resolve({
          session: { id: sid, title: 'Persisted', model: '', provider: '' },
          messages: [],
        })),
    drainFrames: () => {
      state.drained++
    },
    transport: transport.transport,
  })
  return { state, run }
}

describe('foreground run', () => {
  test('routes events, drains + hydrates on done, closes the socket eagerly', async () => {
    const transport = scriptedPostTransport()
    const h = makeForeground({ transport })

    transport.emit({ type: 'text', delta: 'hi' })
    transport.emit({ type: 'done' })
    await settle()

    expect(h.state.events.map((e) => e.type)).toEqual(['text'])
    expect(h.state.drained).toBeGreaterThanOrEqual(1)
    expect(h.state.doneCalls).toBe(1)
    // The socket is closed by the run itself so a detached server stream cannot
    // leave the indicator lit past the final event.
    expect(transport.state.closed).toBe(1)
    expect(h.state.hydrated?.session.id).toBe('sid_A')
  })

  test('dispose silences a late text frame that arrives after the socket aborts', () => {
    const transport = scriptedPostTransport()
    const h = makeForeground({ transport })

    h.run.dispose()
    // The user navigated away; the aborted socket delivers one last frame the
    // server had already buffered. It must NEVER reach the (now-different)
    // consumer.
    transport.emit({ type: 'text', delta: 'STALE' })
    expect(h.state.events).toEqual([])
  })

  test('dispose silences a late done + rehydrate promise (session-switch race)', async () => {
    const transport = scriptedPostTransport()
    let resolveFetch
    const h = makeForeground({
      transport,
      overrides: {
        fetchSession: () =>
          new Promise((res) => {
            resolveFetch = res
          }),
      },
    })

    // A done arrives, hydrate is in flight; user navigates away before the
    // hydrate response comes back.
    transport.emit({ type: 'done' })
    h.run.dispose()

    resolveFetch({
      session: { id: 'sid_A', title: 'Stale', model: '', provider: '' },
      messages: [],
    })
    await settle()

    // done still fired (it ran synchronously with the event) but the async
    // hydrate did NOT reach the consumer — that is what would have overwritten
    // the new session's freshly-loaded transcript.
    expect(h.state.doneCalls).toBe(1)
    expect(h.state.hydrated).toBeNull()
  })

  test('dispose silences a late transport error', () => {
    const transport = scriptedPostTransport()
    const h = makeForeground({ transport })

    h.run.dispose()
    transport.fail(new Error('socket closed'))
    expect(h.state.errorCalls).toBe(0)
  })

  test('dispose silences a late final callback', () => {
    const transport = scriptedPostTransport()
    const h = makeForeground({ transport })

    h.run.dispose()
    transport.finish()
    expect(h.state.finalCalls).toBe(0)
  })

  test('session adoption fires before the event is applied so callers can update owner', () => {
    const transport = scriptedPostTransport()
    const h = makeForeground({ transport, overrides: { adoptSession: true, currentSid: undefined } })

    transport.emit({ type: 'session', id: 'sid_NEW', title: 'Hello' })
    // The onSession callback ran with the adopted id BEFORE the event was
    // handed to the reducer, so a downstream owner-ref update sees the new id
    // by the time the next frame lands.
    expect(h.state.session).toEqual({ id: 'sid_NEW', title: 'Hello' })
    expect(h.state.events.map((e) => e.type)).toEqual(['session'])
    expect(h.state.currentSid).toBe('sid_NEW')
  })

  test('done without a session id skips the hydrate', async () => {
    const transport = scriptedPostTransport()
    const h = makeForeground({ transport, overrides: { currentSid: undefined } })

    transport.emit({ type: 'done' })
    await settle()

    expect(h.state.doneCalls).toBe(1)
    expect(h.state.hydrated).toBeNull()
  })

  test('clean EOF without a done fires onCleanEof so the caller can render an error', () => {
    const transport = scriptedPostTransport()
    const h = makeForeground({ transport })

    // Socket closes cleanly (finish() = onDone from the transport layer) but
    // the server never emitted a protocol `done` event. That is the silent-
    // stop bug the callback exists to surface.
    transport.finish()

    expect(h.state.cleanEofCalls).toBe(1)
    expect(h.state.finalCalls).toBe(1)
    expect(h.state.doneCalls).toBe(0)
    expect(h.state.errorCalls).toBe(0)
  })

  test('clean EOF that follows a `done` does NOT fire onCleanEof', async () => {
    const transport = scriptedPostTransport()
    const h = makeForeground({ transport })

    transport.emit({ type: 'done' })
    await settle()
    transport.finish()

    // The turn finished normally; onFinal fired but onCleanEof stayed silent
    // so the transcript does not paint a spurious error over a clean turn.
    expect(h.state.cleanEofCalls).toBe(0)
    expect(h.state.doneCalls).toBe(1)
  })
})

/* ---------- lifecycle: retained handle semantics ---------- */

describe('foreground run lifecycle', () => {
  test('onSettled fires exactly once after done + hydrate resolve', async () => {
    const transport = scriptedPostTransport()
    let settled = 0
    let resolveFetch
    createForegroundRun({
      assistantId: 'assist_1',
      path: '/chat',
      body: {},
      currentSessionId: () => 'sid_X',
      onEvent: () => {},
      onDone: () => {},
      onError: () => {},
      onFinal: () => {},
      onSettled: () => {
        settled++
      },
      onSessionHydrated: () => {},
      fetchSession: () => new Promise((resolve) => { resolveFetch = resolve }),
      drainFrames: () => {},
      transport: transport.transport,
    })

    transport.emit({ type: 'done' })
    transport.finish()
    expect(settled).toBe(0)
    resolveFetch({ session: { id: 'sid_X', title: '', model: '', provider: '' }, messages: [] })
    await settle()

    expect(settled).toBe(1)
    // A second done from the same run must NEVER re-fire settle (defensive
    // against the transport calling onDone twice on close).
    transport.emit({ type: 'done' })
    await settle()
    expect(settled).toBe(1)

  })

  test('dispose suppresses hydration and settlement from an obsolete run', async () => {
    const transport = scriptedPostTransport()
    const events = []
    let resolveFetch
    const run = createForegroundRun({
      assistantId: 'assist_1',
      path: '/chat',
      body: {},
      currentSessionId: () => 'sid_A',
      onEvent: () => {},
      onDone: () => events.push('done'),
      onError: () => events.push('error'),
      onFinal: () => events.push('final'),
      onSettled: () => events.push('settled'),
      onSessionHydrated: () => events.push('hydrated'),
      fetchSession: () =>
        new Promise((res) => {
          resolveFetch = res
        }),
      drainFrames: () => {},
      transport: transport.transport,
    })

    transport.emit({ type: 'done' })
    // Hydrate is in flight; user navigates away.
    run.dispose()
    expect(events).toEqual(['done'])

    resolveFetch({
      session: { id: 'sid_A', title: 'Stale', model: '', provider: '' },
      messages: [],
    })
    await settle()

    expect(events).toEqual(['done'])
  })

  test('onSettled fires on transport error, before consumer releases handle', () => {
    const transport = scriptedPostTransport()
    const events = []
    createForegroundRun({
      assistantId: 'a',
      path: '/chat',
      body: {},
      currentSessionId: () => 'sid_A',
      onEvent: () => {},
      onDone: () => {},
      onError: () => events.push('error'),
      onFinal: () => {},
      onSettled: () => events.push('settled'),
      onSessionHydrated: () => {},
      fetchSession: () => Promise.resolve({ session: {}, messages: [] }),
      drainFrames: () => {},
      transport: transport.transport,
    })

    transport.fail(new Error('boom'))
    // Owner learns of the error, then gets settled — same order sendText
    // relies on to release the handle without shadowing the user error.
    expect(events).toEqual(['error', 'settled'])
  })

  test('onSettled fires on clean onFinal even without a preceding done', () => {
    const transport = scriptedPostTransport()
    const events = []
    createForegroundRun({
      assistantId: 'a',
      path: '/chat',
      body: {},
      currentSessionId: () => 'sid_A',
      onEvent: () => {},
      onDone: () => events.push('done'),
      onError: () => events.push('error'),
      onFinal: () => events.push('final'),
      onSettled: () => events.push('settled'),
      onSessionHydrated: () => {},
      fetchSession: () => Promise.resolve({ session: {}, messages: [] }),
      drainFrames: () => {},
      transport: transport.transport,
    })

    transport.finish()
    expect(events).toEqual(['final', 'settled'])
  })

  test('single-dispatch: a `session` event triggers onSession exactly once, not twice', () => {
    const transport = scriptedPostTransport()
    const adopted = []
    const routed = []
    createForegroundRun({
      assistantId: 'a',
      path: '/chat',
      body: {},
      currentSessionId: () => undefined,
      onSession: (id, title) => adopted.push({ id, title }),
      onEvent: (event) => routed.push(event.type),
      onDone: () => {},
      onError: () => {},
      onFinal: () => {},
      onSessionHydrated: () => {},
      fetchSession: () => Promise.resolve({ session: {}, messages: [] }),
      drainFrames: () => {},
      transport: transport.transport,
    })

    transport.emit({ type: 'session', id: 'sid_NEW', title: 'Hi' })
    // Adoption fired once; the event also flowed to onEvent so the reducer
    // can update title/etc, but adoption is a single dispatch. This is the
    // regression: earlier wiring double-fired navigate() on brand-new chats.
    expect(adopted).toEqual([{ id: 'sid_NEW', title: 'Hi' }])
    expect(routed).toEqual(['session'])
  })
})
