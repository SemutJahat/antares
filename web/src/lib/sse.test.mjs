import { afterEach, beforeEach, describe, expect, test } from 'bun:test'
import { consumeSseStream } from './sse.ts'
import { ApiError, streamGet, streamPost } from './api.ts'

/**
 * Build a ReadableStream that yields the supplied byte chunks in order.
 * Each element is a Uint8Array or a UTF-8 string; this exercises the same
 * code path a real fetch body takes.
 */
function bytesStream(chunks, { onCancel } = {}) {
  const encoder = new TextEncoder()
  const queue = chunks.map((c) => (typeof c === 'string' ? encoder.encode(c) : c))
  return new ReadableStream({
    pull(controller) {
      const next = queue.shift()
      if (!next) controller.close()
      else controller.enqueue(next)
    },
    cancel(reason) {
      onCancel?.(reason)
    },
  })
}

/**
 * Build a ReadableStream that emits one frame, then waits for `release()`
 * before either enqueuing a follow-up chunk or closing. Lets tests race a
 * dispose against an in-flight parser.
 */
function pausableStream(first, laterOrClose, { onCancel } = {}) {
  let release
  const done = new Promise((r) => {
    release = r
  })
  const stream = new ReadableStream({
    async pull(c) {
      c.enqueue(new TextEncoder().encode(first))
      await done
      try {
        if (typeof laterOrClose === 'string') {
          c.enqueue(new TextEncoder().encode(laterOrClose))
        }
        c.close()
      } catch {
        /* controller already closed by cancel — expected */
      }
    },
    cancel(reason) {
      onCancel?.(reason)
      release?.()
    },
  })
  return { stream, release: () => release?.() }
}

/* ---------- parser behaviour observed through the public byte-stream API ---------- */

describe('SSE parser: consumeSseStream over real byte chunks', () => {
  test('LF-only frames dispatch each event exactly once', async () => {
    const seen = []
    await consumeSseStream(bytesStream(['data: {"n":1}\n\ndata: {"n":2}\n\n']), {
      onEvent: (e) => seen.push(e),
    })
    expect(seen).toEqual([{ n: 1 }, { n: 2 }])
  })

  test('CRLF frames dispatch each event exactly once', async () => {
    const seen = []
    await consumeSseStream(bytesStream(['data: {"n":1}\r\n\r\ndata: {"n":2}\r\n\r\n']), {
      onEvent: (e) => seen.push(e),
    })
    expect(seen).toEqual([{ n: 1 }, { n: 2 }])
  })

  test('lone-CR line terminators are honoured (legacy Mac line endings)', async () => {
    const seen = []
    await consumeSseStream(bytesStream(['data: {"n":1}\r\rdata: {"n":2}\r\r']), {
      onEvent: (e) => seen.push(e),
    })
    expect(seen).toEqual([{ n: 1 }, { n: 2 }])
  })

  test('mixed CRLF then LF terminators in one stream', async () => {
    const seen = []
    await consumeSseStream(
      bytesStream(['data: {"n":1}\r\n\r\ndata: {"n":2}\n\n']),
      { onEvent: (e) => seen.push(e) },
    )
    expect(seen).toEqual([{ n: 1 }, { n: 2 }])
  })

  test('mixed LF then CRLF terminators in one stream', async () => {
    const seen = []
    await consumeSseStream(
      bytesStream(['data: {"n":1}\n\ndata: {"n":2}\r\n\r\n']),
      { onEvent: (e) => seen.push(e) },
    )
    expect(seen).toEqual([{ n: 1 }, { n: 2 }])
  })

  test('CRLF split between CR and LF chunks counts as one terminator', async () => {
    // Terminator `\r\n` split as `\r` | `\n`. Include the blank-line frame
    // separator so each `data:` line dispatches its own event.
    const seen = []
    await consumeSseStream(
      bytesStream(['data: {"n":1}\r', '\n\r\ndata: {"n":2}\r\n\r\n']),
      { onEvent: (e) => seen.push(e) },
    )
    expect(seen).toEqual([{ n: 1 }, { n: 2 }])
  })

  test('blank-line separator split mid-CRLF across chunk boundaries', async () => {
    // Frame terminator `\r\n\r\n` split as `\r\n\r` | `\n…`.
    const seen = []
    await consumeSseStream(
      bytesStream(['data: {"n":1}\r\n\r', '\ndata: {"n":2}\r\n\r\n']),
      { onEvent: (e) => seen.push(e) },
    )
    expect(seen).toEqual([{ n: 1 }, { n: 2 }])
  })

  test('joins UTF-8 sequences split across chunk boundaries', async () => {
    // "π" is 0xCF 0x80 in UTF-8 — split between chunks.
    const seen = []
    await consumeSseStream(
      bytesStream([
        new Uint8Array([0x64, 0x61, 0x74, 0x61, 0x3a, 0x20, 0x22, 0xcf]),
        new Uint8Array([0x80, 0x22, 0x0a, 0x0a]),
      ]),
      { onEvent: (e) => seen.push(e) },
    )
    expect(seen).toEqual(['π'])
  })

  test('joins multi-line data fields with newlines', async () => {
    const seen = []
    await consumeSseStream(bytesStream(['data: {"a":1,\ndata: "b":"c"}\n\n']), {
      onEvent: (e) => seen.push(e),
    })
    expect(seen).toEqual([{ a: 1, b: 'c' }])
  })

  test('skips comment lines and [DONE] sentinels', async () => {
    const seen = []
    await consumeSseStream(
      bytesStream([': keepalive\n\ndata: [DONE]\n\ndata: {"n":1}\n\n']),
      { onEvent: (e) => seen.push(e) },
    )
    expect(seen).toEqual([{ n: 1 }])
  })

  test('strips a single leading space after data: per spec', async () => {
    const seen = []
    // Second frame has no space after the colon — payload preserved as-is.
    await consumeSseStream(bytesStream(['data: "spaced"\n\ndata:"tight"\n\n']), {
      onEvent: (e) => seen.push(e),
    })
    expect(seen).toEqual(['spaced', 'tight'])
  })

  test('routes malformed JSON to onParseError and keeps the stream alive', async () => {
    const seen = []
    const errs = []
    await consumeSseStream(bytesStream(['data: not-json\n\ndata: {"n":1}\n\n']), {
      onEvent: (e) => seen.push(e),
      onParseError: (raw, err) => errs.push([raw, err.name]),
    })
    expect(seen).toEqual([{ n: 1 }])
    expect(errs).toEqual([['not-json', 'SyntaxError']])
  })

  test('discards unfinished trailing record at EOS per SSE spec', async () => {
    const seen = []
    await consumeSseStream(bytesStream(['data: {"n":1}\n\ndata: {"n":2}']), {
      onEvent: (e) => seen.push(e),
    })
    expect(seen).toEqual([{ n: 1 }])
  })

  test('consumer exception cancels the reader and rethrows', async () => {
    const cancels = []
    const bomb = new Error('consumer boom')
    const stream = bytesStream(['data: {"n":1}\n\ndata: {"n":2}\n\n'], {
      onCancel: (r) => cancels.push(r),
    })
    await expect(
      consumeSseStream(stream, {
        onEvent: () => {
          throw bomb
        },
      }),
    ).rejects.toBe(bomb)
    expect(cancels).toEqual([bomb])
  })
})

/* ---------- transport tests: drive actual streamGet/streamPost ---------- */

const originalFetch = globalThis.fetch
const originalLocalStorage = globalThis.localStorage

/** Minimal in-memory Storage stub — real code only touches getItem/…/remove. */
function installStorageStub(initial = {}) {
  const map = new Map(Object.entries(initial))
  globalThis.localStorage = {
    getItem: (k) => (map.has(k) ? map.get(k) : null),
    setItem: (k, v) => map.set(k, String(v)),
    removeItem: (k) => {
      map.delete(k)
    },
    clear: () => map.clear(),
    key: (i) => Array.from(map.keys())[i] ?? null,
    get length() {
      return map.size
    },
  }
}

/**
 * Install a fetch stub that captures the request URL/init and returns a
 * Response built from `bodyStream`. Test picks status/ok. `onRequest` sees
 * `{ url, init }` for assertion.
 */
function installFetchStub({ status = 200, bodyStream, textBody, onRequest, holdMs = 0 } = {}) {
  globalThis.fetch = async (url, init) => {
    onRequest?.({ url, init })
    if (holdMs > 0) {
      // Race window: let the caller abort while fetch itself is pending.
      // Reject early if abort fires during the delay.
      await new Promise((resolve, reject) => {
        const t = setTimeout(resolve, holdMs)
        init?.signal?.addEventListener('abort', () => {
          clearTimeout(t)
          reject(new DOMException('aborted', 'AbortError'))
        })
      })
    }
    if (init?.signal?.aborted) throw new DOMException('aborted', 'AbortError')
    // Wire the AbortSignal to the response body so post-fetch aborts
    // cascade into stream cancellation, mirroring real fetch semantics.
    if (bodyStream && init?.signal) {
      init.signal.addEventListener('abort', () => {
        bodyStream.cancel(new DOMException('aborted', 'AbortError')).catch(() => {})
      })
    }
    const okRange = status >= 200 && status < 300
    if (!okRange) {
      if (bodyStream) return new Response(bodyStream, { status })
      return new Response(textBody ?? '', { status })
    }
    return new Response(bodyStream ?? new ReadableStream({ start: (c) => c.close() }), {
      status,
    })
  }
}

beforeEach(() => {
  installStorageStub({ 'antares.token': 'test-token' })
})

afterEach(() => {
  globalThis.fetch = originalFetch
  if (originalLocalStorage === undefined) delete globalThis.localStorage
  else globalThis.localStorage = originalLocalStorage
})

describe('streamPost / streamGet: real transport behaviour', () => {
  test('streamPost delivers parsed events, fires onDone once, sets JSON + auth headers', async () => {
    let captured
    installFetchStub({
      bodyStream: bytesStream(['data: {"type":"a"}\n\n', 'data: {"type":"b"}\n\n']),
      onRequest: (r) => {
        captured = r
      },
    })
    const events = []
    let dones = 0
    let errs = 0
    await new Promise((resolve) => {
      streamPost(
        '/chat',
        { hello: 'world' },
        (e) => events.push(e),
        () => errs++,
        () => {
          dones++
          resolve()
        },
      )
    })
    expect(events).toEqual([{ type: 'a' }, { type: 'b' }])
    expect(dones).toBe(1)
    expect(errs).toBe(0)
    expect(captured.url).toBe('/api/chat')
    expect(captured.init.method).toBe('POST')
    expect(captured.init.credentials).toBe('include')
    expect(captured.init.headers['Content-Type']).toBe('application/json')
    expect(captured.init.headers.Authorization).toBe('Bearer test-token')
    expect(captured.init.body).toBe(JSON.stringify({ hello: 'world' }))
  })

  test('streamGet omits legacy ?token= and relies on Authorization header', async () => {
    let captured
    installFetchStub({
      bodyStream: bytesStream(['data: {"type":"ok"}\n\n']),
      onRequest: (r) => {
        captured = r
      },
    })
    await new Promise((resolve) => {
      streamGet('/chat/attach?session_id=abc', () => {}, undefined, resolve)
    })
    expect(captured.url).toBe('/api/chat/attach?session_id=abc')
    expect(captured.url).not.toContain('token=')
    expect(captured.init.headers.Authorization).toBe('Bearer test-token')
    expect(captured.init.headers.Accept).toBe('text/event-stream')
    expect(captured.init.credentials).toBe('include')
  })

  test('consumer exception in streamPost reaches onError (not swallowed)', async () => {
    installFetchStub({
      bodyStream: bytesStream(['data: {"type":"x"}\n\n', 'data: {"type":"y"}\n\n']),
    })
    let errMessage
    let dones = 0
    await new Promise((resolve) => {
      streamPost(
        '/chat',
        null,
        (e) => {
          if (e.type === 'x') throw new Error('consumer boom')
        },
        (err) => {
          errMessage = err.message
          resolve()
        },
        () => dones++,
      )
    })
    expect(errMessage).toBe('consumer boom')
    expect(dones).toBe(0)
  })

  test('non-ok HTTP response routes ApiError to onError', async () => {
    installFetchStub({ status: 500, textBody: '{"error":"nope"}' })
    let captured
    await new Promise((resolve) => {
      streamGet(
        '/chat/attach',
        () => {},
        (err) => {
          captured = err
          resolve()
        },
      )
    })
    expect(captured).toBeInstanceOf(ApiError)
    expect(captured.status).toBe(500)
  })

  test('abort suppresses later events and calls onDone once', async () => {
    const { stream, release } = pausableStream(
      'data: {"type":"first"}\n\n',
      'data: {"type":"late"}\n\n',
    )
    installFetchStub({ bodyStream: stream })

    const events = []
    let dones = 0
    let errs = 0
    let abort
    const finished = new Promise((resolve) => {
      abort = streamGet(
        '/chat/attach',
        (e) => events.push(e),
        () => errs++,
        () => {
          dones++
          resolve()
        },
      )
    })

    for (let i = 0; i < 200 && events.length === 0; i++) {
      await new Promise((r) => setTimeout(r, 1))
    }
    abort()
    // Let the pumped follow-up frame arrive; the transport MUST drop it.
    release()
    await finished

    expect(events).toEqual([{ type: 'first' }])
    expect(dones).toBe(1)
    expect(errs).toBe(0)
  })

  test('abort while response.text is pending never invokes onError', async () => {
    // Body whose pull hangs until the request's AbortSignal fires, then
    // errors — mirrors real fetch: an in-flight body read fails on abort.
    let abortSignal
    const infinite = new ReadableStream({
      pull() {
        return new Promise((_resolve, reject) => {
          abortSignal?.addEventListener('abort', () => {
            reject(new DOMException('aborted', 'AbortError'))
          })
        })
      },
    })
    // Intercept fetch to capture the signal used by streamRequest so pull
    // can react to it, mirroring the browser plumbing.
    globalThis.fetch = async (url, init) => {
      abortSignal = init?.signal
      return new Response(infinite, { status: 500 })
    }

    let errs = 0
    let dones = 0
    let abort
    const finished = new Promise((resolve) => {
      abort = streamGet(
        '/chat/attach',
        () => {},
        () => {
          errs++
          resolve()
        },
        () => {
          dones++
          resolve()
        },
      )
    })
    // Let the fetch resolve and streamRequest enter res.text().
    await new Promise((r) => setTimeout(r, 5))
    abort()
    await finished

    // The only user-visible contract: onError MUST NOT fire, onDone fires
    // exactly once. Stream cancellation propagation is an implementation
    // detail of the platform's fetch.
    expect(errs).toBe(0)
    expect(dones).toBe(1)
  })

  test('abort while fetch itself is pending never invokes onError', async () => {
    installFetchStub({
      bodyStream: bytesStream(['data: {"type":"n"}\n\n']),
      holdMs: 30,
    })
    let errs = 0
    let dones = 0
    let abort
    const finished = new Promise((resolve) => {
      abort = streamGet(
        '/chat/attach',
        () => {},
        () => {
          errs++
          resolve()
        },
        () => {
          dones++
          resolve()
        },
      )
    })
    await new Promise((r) => setTimeout(r, 5))
    abort()
    await finished
    expect(errs).toBe(0)
    expect(dones).toBe(1)
  })
})
