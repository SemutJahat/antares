/**
 * Framework-agnostic chat streaming logic — event application and the standing
 * attach lifecycle — extracted from ChatPage so the reducer and the reconnect
 * loop can be exercised deterministically without React or a real network.
 *
 * The controller talks to the outside world only through injected callbacks
 * (state setters, transcript hydration) and deps (transport, fetch, timers).
 * `useChatStream` wires the React setters and the real transport in.
 */
import {
  appendSeg,
  normalizeErrorPayload,
  pushToolSeg,
  updateToolSeg,
  type ChatMessage,
  type SessionDetail,
} from './chatTranscript'
import {
  groupStreamPatches,
  queueStreamDelta,
  shouldRefreshAfterAttach,
  type QueuedStreamPatch,
} from './chatStreamQueue'

/** Only the fields the controller reads. Widened to the api.StreamEvent shape. */
export interface StreamEventLike {
  type?: string
  cursor?: number
  id?: string
  title?: string
  name?: string
  arguments?: string
  delta?: string
  chunk?: string
  message?: string
  content?: string
  is_error?: boolean
  input_tokens?: number
  output_tokens?: number
  context_tokens?: number
  context_window?: number
  error?: string
}

export interface LiveStatus {
  turn: number
  tool?: string
  waiting?: boolean
  notice?: string
}

export interface UsageInfo {
  used?: number
  window?: number
}

/**
 * The callbacks a single applyStreamEvent invocation may fire. Every UI-side
 * setter reaches the controller through this envelope, so the reducer is a
 * pure function of (event, callbacks).
 */
export interface EventCallbacks {
  patchAssistant: (fn: (m: ChatMessage) => ChatMessage) => void
  appendDelta: (segment: 'text' | 'reasoning', delta: string) => void
  setLive: (updater: (s: LiveStatus) => LiveStatus) => void
  setAskId: (id: string) => void
  setUsage: (u: UsageInfo) => void
  setTitle: (title: string) => void
  onSession?: (id: string, title?: string) => void
  showReasoning: () => boolean
  errorMessage: () => string
}

/**
 * Apply one stream event. Shared by the foreground turn and the standing
 * attach; the caller supplies the callbacks, so identical inputs render
 * identically no matter which transport drove the event.
 *
 * Text/reasoning deltas go through `appendDelta` (routed to the frame
 * batcher) so a replay burst is coalesced rather than committed per token.
 * All other events run through `patchAssistant` so they share the same
 * flush frame.
 */
export function applyStreamEvent(
  event: StreamEventLike,
  cb: EventCallbacks,
): void {
  switch (event.type) {
    case 'session':
      cb.onSession?.(
        typeof event.id === 'string' ? event.id : '',
        typeof event.title === 'string' ? event.title : undefined,
      )
      break
    case 'text':
      cb.appendDelta('text', String(event.delta ?? ''))
      break
    case 'reasoning':
      // Honour display.show_reasoning even if a stale event arrives — the
      // server also suppresses when false, this keeps the UI consistent.
      if (cb.showReasoning()) {
        cb.appendDelta('reasoning', String(event.delta ?? ''))
      }
      break
    case 'tool_call':
      cb.setLive((s) => ({
        turn: s.turn + 1,
        tool: String(event.name ?? ''),
        notice: undefined,
      }))
      cb.patchAssistant((m) =>
        pushToolSeg(m, {
          id: String(event.id ?? ''),
          name: String(event.name ?? ''),
          args: String(event.arguments ?? ''),
          running: true,
        }),
      )
      break
    case 'tool_progress':
      cb.patchAssistant((m) =>
        updateToolSeg(m, String(event.id ?? ''), (c) => ({
          ...c,
          // A chunk streams (append); a message is a status line (replace),
          // so "attempt 1/3" → "attempt 2/3" swaps in place instead of
          // concatenating.
          progress:
            event.chunk != null
              ? (c.progress ?? '') + String(event.chunk)
              : String(event.message ?? c.progress ?? ''),
        })),
      )
      break
    case 'tool_result':
      cb.setLive((s) => ({ ...s, tool: undefined }))
      cb.patchAssistant((m) =>
        updateToolSeg(m, String(event.id ?? ''), (c) => ({
          ...c,
          result: String(event.content ?? ''),
          isError: !!event.is_error,
          running: false,
        })),
      )
      break
    case 'notice':
      cb.setLive((s) => ({
        ...s,
        notice: String(event.message ?? event.content ?? '').trim() || undefined,
      }))
      break
    case 'ask':
      cb.setAskId(String(event.id ?? ''))
      cb.setLive((s) => ({ ...s, tool: undefined, waiting: true, notice: undefined }))
      break
    case 'usage':
      cb.patchAssistant((m) => ({
        ...m,
        tokensIn: Number(event.input_tokens ?? m.tokensIn ?? 0),
        tokensOut: Number(event.output_tokens ?? m.tokensOut ?? 0),
      }))
      {
        const win = Number(event.context_window ?? 0)
        const used = Number(event.context_tokens ?? 0)
        const usage: UsageInfo = {}
        if (used > 0) usage.used = used
        if (win > 0) usage.window = win
        if (usage.used || usage.window) cb.setUsage(usage)
      }
      break
    case 'reset':
      // The turn is being retried after a provider glitch — throw away the
      // partial reply so the retry does not render on top of it.
      cb.patchAssistant((m) => ({
        ...m,
        content: '',
        reasoning: undefined,
        toolCalls: undefined,
        segments: [],
        error: undefined,
        errorSource: undefined,
      }))
      break
    case 'error':
      cb.patchAssistant((m) => ({
        ...m,
        error: normalizeErrorPayload(event.error ?? cb.errorMessage()),
        errorSource: 'server',
      }))
      break
  }
}

/* ---------- Frame batcher ---------- */

export interface FrameSchedule {
  schedule: (fn: () => void) => number
  cancel: (handle: number) => void
}

export interface FrameBatcher {
  enqueuePatch: (id: string, fn: (m: ChatMessage) => ChatMessage) => void
  enqueueDelta: (id: string, segment: 'text' | 'reasoning', delta: string) => void
  drain: () => void
  dispose: () => void
}

export interface FrameBatcherOptions {
  schedule: FrameSchedule
  setMessages: (updater: (prev: ChatMessage[]) => ChatMessage[]) => void
  maxLiveReasoningChars: () => number
}

/**
 * Coalesce many small stream events into one React commit per animation frame.
 * A turn emits hundreds of tiny deltas; committing each one would render every
 * bubble on every token. Adjacent same-segment deltas are also merged in the
 * queue so a replay burst becomes a single string append, not thousands.
 */
export function createFrameBatcher(opts: FrameBatcherOptions): FrameBatcher {
  const queue: QueuedStreamPatch<ChatMessage>[] = []
  let handle: number | null = null

  const flush = () => {
    handle = null
    if (queue.length === 0) return
    const patches = queue.splice(0, queue.length)
    const byMessage = groupStreamPatches(patches)
    opts.setMessages((prev) => {
      if (byMessage.size === 0) return prev
      let next = prev
      let cloned = false
      for (const [id, list] of byMessage) {
        const idx = next.findIndex((m) => m.id === id)
        if (idx < 0) continue
        let message = next[idx]
        const cap = opts.maxLiveReasoningChars()
        for (const patch of list) {
          message =
            patch.kind === 'delta'
              ? appendSeg(message, patch.segment, patch.delta, cap)
              : patch.fn(message)
        }
        if (!cloned) {
          next = prev.slice()
          cloned = true
        }
        next[idx] = message
      }
      return next
    })
  }

  const ensureScheduled = () => {
    if (handle == null) handle = opts.schedule.schedule(flush)
  }

  return {
    enqueuePatch(id, fn) {
      queue.push({ id, kind: 'apply', fn })
      ensureScheduled()
    },
    enqueueDelta(id, segment, delta) {
      if (!delta) return
      queueStreamDelta(queue, id, segment, delta)
      ensureScheduled()
    },
    drain() {
      if (handle != null) {
        opts.schedule.cancel(handle)
        handle = null
      }
      flush()
    },
    dispose() {
      if (handle != null) {
        opts.schedule.cancel(handle)
        handle = null
      }
      queue.length = 0
    },
  }
}

/* ---------- Standing attach ---------- */

export type StreamCloser = () => void
export type StreamGetLike = (
  path: string,
  onEvent: (event: StreamEventLike) => void,
  onError: (err: unknown) => void,
) => StreamCloser

export interface AttachCallbacks {
  /** True while a foreground send is streaming — attach must wait it out. */
  isForegroundActive: () => boolean
  ensureAssistant: () => string
  onDone: (assistantWasLive: boolean) => void
  onSessionHydrated: (detail: SessionDetail) => void
  onAuthFailure: () => void
  onEvent: (assistantId: string, event: StreamEventLike) => void
  fetchSession: (sid: string) => Promise<SessionDetail>
  transport: StreamGetLike
  setTimer: (fn: () => void, ms: number) => number
  clearTimer: (handle: number) => void
  /** Injected classifier so tests can raise a synthetic auth failure. */
  isAuthError?: (err: unknown) => boolean
  attachRetryMs?: number
  attachIdleMs?: number
}

export interface StandingAttach {
  dispose: () => void
}

/**
 * Reconnect to a live turn on this session and keep the reconnection standing
 * so a server-initiated turn later (e.g. a background sub-agent finishing)
 * streams in without a page refresh. Every event and async result checks
 * `alive` first: a switched session must never overwrite the new transcript.
 *
 * Cursor is deduped locally. The backend cursor is the NEXT event index; the
 * server may replay events across a reconnection window, so we skip any event
 * whose cursor we already applied.
 */
export function createStandingAttach(
  sid: string,
  cb: AttachCallbacks,
): StandingAttach {
  let alive = true
  let close: StreamCloser | undefined
  let assistantId: string | null = null
  /** Highest cursor already applied. A duplicate replay has cursor <= this. */
  let lastAppliedCursor = -1
  /** Cursor requested from the backend for the next connect. */
  let requestCursor = 0
  let idleRefreshDone = false
  let pendingTimer: number | null = null
  const retryMs = cb.attachRetryMs ?? 3000
  const idleMs = cb.attachIdleMs ?? 1500

  const scheduleNext = (delay: number) => {
    if (!alive) return
    if (pendingTimer != null) cb.clearTimer(pendingTimer)
    pendingTimer = cb.setTimer(() => {
      pendingTimer = null
      connect()
    }, delay)
  }

  const connect = () => {
    if (!alive) return
    // A foreground send owns its own stream; running two in parallel would
    // double-render the turn. Retry shortly.
    if (cb.isForegroundActive()) {
      scheduleNext(idleMs)
      return
    }
    close = cb.transport(
      `/chat/attach?session_id=${encodeURIComponent(sid)}&cursor=${requestCursor}`,
      (event) => {
        if (!alive) return
        if (event.type === 'done') {
          const hadEvents = assistantId !== null
          const shouldRefresh = shouldRefreshAfterAttach(hadEvents, idleRefreshDone)
          assistantId = null
          idleRefreshDone = true
          // A later server-initiated turn is a fresh live run with its own
          // zero-based cursor. Reset only after done, and only after the
          // hydrate promise settles below.
          requestCursor = 0
          lastAppliedCursor = -1
          cb.onDone(hadEvents)
          close?.()
          const after = shouldRefresh
            ? cb
                .fetchSession(sid)
                .then((detail) => {
                  if (!alive) return
                  cb.onSessionHydrated(detail)
                })
                .catch(() => {
                  /* stale response — next connect will retry hydration */
                })
            : Promise.resolve()
          void after.finally(() => scheduleNext(idleMs))
          return
        }
        // Backend cursor is the NEXT event index. If it is <= lastApplied we
        // have replayed this frame across a reconnect — skip so a duplicate
        // token does not appear on screen.
        const eventCursor = Number(event.cursor ?? NaN)
        const hasCursor = Number.isFinite(eventCursor)
        if (hasCursor && eventCursor <= lastAppliedCursor) return
        if (hasCursor) {
          lastAppliedCursor = eventCursor
          // Ask the backend to resume after the latest applied event on the
          // next reconnect.
          requestCursor = eventCursor
        }
        const id = assistantId ?? (assistantId = cb.ensureAssistant())
        cb.onEvent(id, event)
      },
      (err) => {
        if (!alive) return
        close?.()
        // A 401 will not fix itself; stop the retry loop and surface it.
        if (cb.isAuthError?.(err)) {
          cb.onAuthFailure()
          return
        }
        scheduleNext(retryMs)
      },
    )
  }

  connect()

  return {
    dispose() {
      alive = false
      if (pendingTimer != null) {
        cb.clearTimer(pendingTimer)
        pendingTimer = null
      }
      close?.()
    },
  }
}

/* ---------- Foreground run ---------- */

/** POST-flavoured transport, mirroring streamPost's shape. */
export type StreamPostLike = (
  path: string,
  body: unknown,
  onEvent: (event: StreamEventLike) => void,
  onError: (err: Error) => void,
  onDone: () => void,
) => StreamCloser

export interface ForegroundRunCallbacks {
  /** Optimistic assistant bubble id — the run applies every event to this id. */
  assistantId: string
  /** Route event through the frame batcher (usually applyStreamEvent). */
  onEvent: (event: StreamEventLike) => void
  /** Called on backend `done`, before hydrate. Callers usually clear live/ask. */
  onDone: () => void
  /** Called on transport error, before dispose. */
  onError: (err: Error) => void
  /** Called after the socket closes cleanly (post-`done` cleanup). */
  onFinal: () => void
  /** Socket closed cleanly with NO `done` and no prior error. Owners paint a
   *  terminal error so the turn does not silently vanish. */
  onCleanEof?: () => void
  /**
   * Fired exactly once, after the run has fully quiesced: on `done`, after
   * the post-done hydrate promise resolves/rejects; on transport error, right
   * after `onError`; on `onFinal` when no `done` preceded (defensive).
   * `onDone` runs BEFORE the async hydrate; owners that need to release their
   * handle after the last state change MUST wait for this instead so a
   * navigation between `done` and hydrate resolve can still dispose() the run.
   */
  onSettled?: () => void
  /** Server-assigned session adoption (persist url, remember last, etc.). */
  onSession?: (id: string, title?: string) => void
  /** Canonical rehydration after done — noop for unauthed clients. */
  fetchSession: (sid: string) => Promise<SessionDetail>
  /** Deliver the hydrated transcript back into React state. */
  onSessionHydrated: (detail: SessionDetail) => void
  /** Drain any pending frames before we stop rendering. */
  drainFrames: () => void
  /** The transport that opens the streaming POST. */
  transport: StreamPostLike
  /** Resolve the session id at each event (may change mid-stream on adoption). */
  currentSessionId: () => string | undefined
  /** URL path (`/chat`). */
  path: string
  /** POST body. */
  body: unknown
}

export interface ForegroundRun {
  /** Assistant bubble id — stable for the run's lifetime. */
  readonly assistantId: string
  /** Abort the transport AND silence every pending async callback. */
  dispose: () => void
}

/**
 * Own one foreground /chat streaming turn end-to-end. Every callback (event,
 * error, final, and the post-done rehydrate promise) is fenced behind an
 * `alive` flag: once `dispose()` runs, a late frame from the aborted socket
 * can no longer mutate state that now belongs to another session.
 *
 * This is what closes the switch-session race the standing attach already
 * handles: navigate from A to B while A is streaming and dispose(); the last
 * text/reasoning/done/error frame from A must NEVER apply to B.
 */
export function createForegroundRun(cb: ForegroundRunCallbacks): ForegroundRun {
  let alive = true
  let settled = false
  let doneSeen = false
  let hydrateInFlight = false
  let close: StreamCloser | undefined

  /**
   * Fire onSettled exactly once, and NEVER from a disposed run. dispose()
   * clears `alive` before touching the socket, so any transport callback that
   * lands after dispose (a queued micro/macro task) short-circuits here and
   * cannot stomp on refs the owner has already reassigned to a fresh run.
   *
   * Callers who initiated dispose owned cleanup themselves — echoing an
   * onSettled back would either double-run their clear or, worse, wipe refs
   * that now belong to whatever they started next.
   */
  const settleOnce = () => {
    if (settled || !alive) return
    settled = true
    alive = false
    cb.onSettled?.()
  }

  close = cb.transport(
    cb.path,
    cb.body,
    (event) => {
      if (!alive || doneSeen) return
      if (event.type === 'done') {
        doneSeen = true
        hydrateInFlight = true
        cb.drainFrames()
        cb.onDone()
        // Close the socket eagerly: a detached run keeps the connection open
        // past the final event, and without this the indicator stayed lit.
        close?.()
        const sid = cb.currentSessionId()
        if (sid) {
          void cb
            .fetchSession(sid)
            .then((detail) => {
              if (!alive) return
              cb.onSessionHydrated(detail)
            })
            .catch(() => {
              /* rehydrate is best-effort; a later attach done retries it */
            })
            .finally(() => {
              hydrateInFlight = false
              // Only settle when still alive — dispose during hydrate has
              // already flipped `alive` false, and echoing settle back to a
              // consumer that moved on would wipe their fresh run's refs.
              settleOnce()
            })
        } else {
          settleOnce()
        }
        return
      }
      // Session adoption first (so callers can update currentSessionId before
      // the next event lands), then hand the event to the batcher.
      if (event.type === 'session' && cb.onSession) {
        cb.onSession(
          typeof event.id === 'string' ? event.id : '',
          typeof event.title === 'string' ? event.title : undefined,
        )
      }
      cb.onEvent(event)
    },
    (err) => {
      if (!alive || doneSeen) return
      cb.drainFrames()
      cb.onError(err)
      settleOnce()
    },
    () => {
      if (!alive) return
      cb.drainFrames()
      cb.onFinal()
      // A clean socket close with a prior `done` leaves the hydrate promise
      // in flight — settling here would drop the handle before hydrate can
      // apply. Let the hydrate finally handle settlement in that case.
      if (doneSeen && hydrateInFlight) return
      // Clean EOF with no `done` and no prior error: the server pipe ended
      // without declaring the turn finished. Surface it as a visible error
      // so the transcript never silently stops mid-turn.
      if (!doneSeen) cb.onCleanEof?.()
      settleOnce()
    },
  )

  return {
    assistantId: cb.assistantId,
    dispose() {
      // Order matters: flip alive BEFORE closing so any transport callback
      // scheduled by close() short-circuits every guard above and cannot
      // reach a caller who now owns a different run.
      alive = false
      settled = true
      close?.()
    },
  }
}

