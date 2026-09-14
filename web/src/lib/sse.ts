/**
 * Server-Sent Events stream parsing.
 *
 * Reads a `ReadableStream<Uint8Array>` incrementally per the HTML SSE spec:
 * lines terminate on CRLF, LF, or a lone CR (an isolated `\r` counts as a
 * terminator, and a `\r` followed by `\n` counts as one terminator, not two);
 * a blank line dispatches the accumulated `data:` buffer; comment lines
 * beginning with `:` are ignored; `data:` values join with `\n` and a single
 * leading space after the colon is stripped; `[DONE]` sentinels are dropped.
 *
 * The parser surfaces successfully-parsed `T` values via `onEvent`, routes
 * malformed JSON payloads to `onParseError`, and lets any exception thrown
 * inside a consumer callback bubble out so the caller can classify it. On
 * that path the reader is cancelled (while still locked) before the
 * exception is rethrown, so the network side is torn down cleanly.
 */

export interface SseParserOptions<T> {
  onEvent: (event: T) => void
  onParseError?: (raw: string, err: Error) => void
}

/**
 * Consume `stream` until end-of-stream, invoking `onEvent` for each JSON
 * payload. Returns normally on graceful EOS; per the SSE spec an unfinished
 * trailing record with no dispatching blank line is discarded.
 */
export async function consumeSseStream<T>(
  stream: ReadableStream<Uint8Array>,
  opts: SseParserOptions<T>,
): Promise<void> {
  const reader = stream.getReader()
  const decoder = new TextDecoder('utf-8')
  // Per SSE spec: a CR at buffer end may pair with a LF in the next chunk to
  // form a single CRLF terminator. `pendingCR` tracks that dangling CR so
  // we don't dispatch a spurious blank line.
  let buffer = ''
  let pendingCR = false
  const state: DispatchState = { dataLines: [] }
  let cancelled = false

  try {
    for (;;) {
      const { done, value } = await reader.read()
      if (done) {
        // Flush any pending multi-byte UTF-8 sequence, then close out.
        buffer += decoder.decode()
        // A dangling CR at EOS is a lone-CR line terminator (no LF ever
        // arrived to pair with it). Fold it back so consumeLines treats it
        // as the end of the current line and the following empty line
        // dispatches any accumulated data.
        if (pendingCR) buffer += '\r'
        consumeLines(buffer, false, state, opts)
        // Per spec: the unfinished record still in `state` at EOS is dropped.
        return
      }

      let chunk = decoder.decode(value, { stream: true })
      if (pendingCR) {
        // Fold the dangling CR back onto the front of the new chunk so
        // consumeLines treats `\r\n` at the boundary as one terminator.
        chunk = '\r' + chunk
        pendingCR = false
      }
      // If this chunk ends with a bare CR, defer it: the next chunk may
      // begin with LF and form CRLF. Only defer when the very last char is
      // CR — otherwise CRs mid-chunk are ordinary terminators.
      if (chunk.length > 0 && chunk.charCodeAt(chunk.length - 1) === 0x0d) {
        pendingCR = true
        chunk = chunk.slice(0, -1)
      }
      buffer = consumeLines(buffer + chunk, true, state, opts)
    }
  } catch (err) {
    // Consumer callback threw, or the reader errored (abort included).
    // Cancel through the still-locked reader — releasing first then
    // cancelling the body races the browser into an "already locked"
    // TypeError — then rethrow so the transport can classify.
    cancelled = true
    try {
      await reader.cancel(err)
    } catch {
      /* already cancelled or errored */
    }
    throw err
  } finally {
    if (!cancelled) {
      try {
        reader.releaseLock()
      } catch {
        /* already released or errored */
      }
    }
  }
}

interface DispatchState {
  // Reserved slot for future SSE fields (event/id/retry). Only `data:` is
  // consumed today because every daemon endpoint dispatches JSON that way.
  dataLines: string[]
}

/**
 * Walk `buffer` line-by-line, handling `\r\n`, `\n`, and lone `\r` as line
 * terminators. Complete lines feed the dispatcher; a blank line flushes the
 * accumulated `data:` buffer. When `partial` is true the trailing unfinished
 * fragment is returned to be prepended to the next chunk; when false, the
 * remainder is processed as one final complete line (used at EOS).
 */
function consumeLines<T>(
  buffer: string,
  partial: boolean,
  state: DispatchState,
  opts: SseParserOptions<T>,
): string {
  let i = 0
  let lineStart = 0
  while (i < buffer.length) {
    const c = buffer.charCodeAt(i)
    if (c === 0x0a /* LF */) {
      handleLine(buffer.slice(lineStart, i), state, opts)
      i += 1
      lineStart = i
      continue
    }
    if (c === 0x0d /* CR */) {
      handleLine(buffer.slice(lineStart, i), state, opts)
      // CRLF is one terminator: swallow the following LF if present.
      if (i + 1 < buffer.length && buffer.charCodeAt(i + 1) === 0x0a) i += 2
      else i += 1
      lineStart = i
      continue
    }
    i += 1
  }
  const tail = buffer.slice(lineStart)
  if (partial) return tail
  // EOS path: any remaining tail is a final complete line.
  if (tail.length > 0) handleLine(tail, state, opts)
  return ''
}

function handleLine<T>(
  line: string,
  state: DispatchState,
  opts: SseParserOptions<T>,
): void {
  if (line.length === 0) {
    dispatch(state, opts)
    return
  }
  if (line.charCodeAt(0) === 0x3a /* ':' */) return // comment
  // Split on the first colon per SSE spec.
  const colon = line.indexOf(':')
  const field = colon === -1 ? line : line.slice(0, colon)
  let value = colon === -1 ? '' : line.slice(colon + 1)
  if (value.length > 0 && value.charCodeAt(0) === 0x20 /* space */) {
    value = value.slice(1)
  }
  if (field === 'data') {
    state.dataLines.push(value)
    return
  }
  // `event:`, `id:`, `retry:` — unused; every server here dispatches JSON via
  // `data:` and the client does not need last-event-id or reconnect timing.
}

function dispatch<T>(state: DispatchState, opts: SseParserOptions<T>): void {
  if (state.dataLines.length === 0) return
  const payload = state.dataLines.join('\n')
  state.dataLines = []
  if (payload === '[DONE]') return
  let parsed: T
  try {
    parsed = JSON.parse(payload) as T
  } catch (err) {
    // Malformed frame: report but keep the stream alive. Consumer exceptions
    // MUST NOT be swallowed here — they bubble out of consumeSseStream.
    opts.onParseError?.(payload, err as Error)
    return
  }
  opts.onEvent(parsed)
}
