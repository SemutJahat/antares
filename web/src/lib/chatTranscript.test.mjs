import { describe, expect, test } from 'bun:test'
import {
  appendSeg,
  hydrate,
  mergeHydratedWithLocalErrors,
  normalizeErrorPayload,
  pushToolSeg,
  updateToolSeg,
} from './chatTranscript.ts'

describe('appendSeg', () => {
  test('coalesces adjacent same-kind deltas into one segment', () => {
    const start = { id: 'a', role: 'assistant', content: '' }
    const one = appendSeg(start, 'text', 'Hel')
    const two = appendSeg(one, 'text', 'lo')
    expect(two.segments).toEqual([{ kind: 'text', text: 'Hello' }])
    expect(two.content).toBe('Hello')
  })

  test('reasoning cap keeps a trailing window and unlimited when 0', () => {
    let m = { id: 'a', role: 'assistant', content: '' }
    for (let i = 0; i < 100; i++) m = appendSeg(m, 'reasoning', 'x', 10)
    expect(m.reasoning?.length).toBe(10)

    let n = { id: 'a', role: 'assistant', content: '' }
    for (let i = 0; i < 100; i++) n = appendSeg(n, 'reasoning', 'x', 0)
    expect(n.reasoning?.length).toBe(100)
  })
})

describe('pushToolSeg / updateToolSeg', () => {
  test('pushToolSeg appends to segments and toolCalls together', () => {
    const start = { id: 'a', role: 'assistant', content: '' }
    const after = pushToolSeg(start, { id: 't1', name: 'read', args: '{}' })
    expect(after.toolCalls).toEqual([{ id: 't1', name: 'read', args: '{}' }])
    expect(after.segments?.[0]).toMatchObject({
      kind: 'tool',
      call: { id: 't1', name: 'read' },
    })
  })

  test('updateToolSeg mutates the call in both places', () => {
    const start = pushToolSeg(
      { id: 'a', role: 'assistant', content: '' },
      { id: 't1', name: 'read', args: '{}', running: true },
    )
    const done = updateToolSeg(start, 't1', (c) => ({ ...c, result: 'ok', running: false }))
    expect(done.toolCalls?.[0]).toMatchObject({ result: 'ok', running: false })
    expect(done.segments?.[0]).toMatchObject({
      kind: 'tool',
      call: { result: 'ok', running: false },
    })
  })
})

describe('hydrate', () => {
  test('rebuilds an assistant message with interleaved text/reasoning/tool segments', () => {
    const out = hydrate({
      session: { id: 's1', title: 'T', model: '', provider: '' },
      messages: [
        {
          id: 'u1',
          role: 'user',
          content: 'hi',
          created_at: '',
          tokens_in: 0,
          tokens_out: 0,
        },
        {
          id: 'a1',
          role: 'assistant',
          content: 'hello',
          reasoning: 'thinking',
          tool_calls: JSON.stringify([{ id: 't1', name: 'read', arguments: '{}' }]),
          created_at: '',
          tokens_in: 10,
          tokens_out: 5,
        },
        {
          id: 't1',
          role: 'tool',
          content: 'ok',
          tool_call_id: 't1',
          created_at: '',
          tokens_in: 0,
          tokens_out: 0,
        },
      ],
    })
    // The tool row folds back into the assistant's tool call — it doesn't
    // appear as a separate message.
    expect(out).toHaveLength(2)
    expect(out[1].toolCalls?.[0]).toMatchObject({
      id: 't1',
      name: 'read',
      result: 'ok',
      running: false,
    })
    // Reasoning first, then text, then tool — the persisted order the model saw.
    expect(out[1].segments?.map((s) => s.kind)).toEqual(['reasoning', 'text', 'tool'])
  })

  test('preserves is_error from message.meta so a reload keeps the red badge', () => {
    const out = hydrate({
      session: { id: 's1', title: '', model: '', provider: '' },
      messages: [
        {
          id: 'a1',
          role: 'assistant',
          content: '',
          tool_calls: JSON.stringify([{ id: 't1', name: 'write', arguments: '{}' }]),
          created_at: '',
          tokens_in: 0,
          tokens_out: 0,
        },
        {
          id: 'tr1',
          role: 'tool',
          content: 'permission denied',
          tool_call_id: 't1',
          created_at: '',
          tokens_in: 0,
          tokens_out: 0,
          meta: { is_error: true },
        },
      ],
    })
    expect(out[0].toolCalls?.[0]).toMatchObject({
      id: 't1',
      result: 'permission denied',
      isError: true,
      running: false,
    })
  })

  test('drops hidden messages so injected sub-agent results never render', () => {
    const out = hydrate({
      session: { id: 's1', title: '', model: '', provider: '' },
      messages: [
        {
          id: 'a1',
          role: 'assistant',
          content: 'visible',
          created_at: '',
          tokens_in: 0,
          tokens_out: 0,
        },
        {
          id: 'a2',
          role: 'assistant',
          content: 'hidden context',
          created_at: '',
          tokens_in: 0,
          tokens_out: 0,
          hidden: true,
        },
      ],
    })
    expect(out).toHaveLength(1)
    expect(out[0].content).toBe('visible')
  })
  test('assistant meta.is_error routes JSON content into message.error, blanks content', () => {
    const out = hydrate({
      session: { id: 's1', title: '', model: '', provider: '' },
      messages: [
        {
          id: 'u1',
          role: 'user',
          content: 'do the thing',
          created_at: '',
          tokens_in: 0,
          tokens_out: 0,
        },
        {
          id: 'a1',
          role: 'assistant',
          content: '{"error":"provider refused: context exceeded"}',
          created_at: '',
          tokens_in: 0,
          tokens_out: 0,
          meta: { is_error: true },
        },
      ],
    })
    expect(out).toHaveLength(2)
    const err = out[1]
    expect(err.content).toBe('')
    // Payload is normalised: single-key JSON with a two-space indent, so a
    // hydrated bubble and a live one render byte-identical text.
    expect(err.error).toBe(
      JSON.stringify({ error: 'provider refused: context exceeded' }, null, 2),
    )
    expect(err.errorSource).toBe('server')
    // Content is blank, so nothing falls through the markdown path or gets
    // copied by the assistant Copy button.
    expect(err.segments).toBeUndefined()
  })
})

describe('normalizeErrorPayload', () => {
  test('unwraps a JSON {error} carrier and re-serializes to the canonical shape', () => {
    expect(normalizeErrorPayload('{"error":"boom"}')).toBe(
      JSON.stringify({ error: 'boom' }, null, 2),
    )
  })

  test('wraps a plain-text message as {error:"..."}', () => {
    expect(normalizeErrorPayload('network unreachable')).toBe(
      JSON.stringify({ error: 'network unreachable' }, null, 2),
    )
  })

  test('strips every other field from a multi-key JSON body so only {error} is visible', () => {
    // Even if the server sends a fatter object, the rendered/copied payload
    // MUST stay a single-key {error} — no `source`, `status`, `stack` leak.
    const payload = normalizeErrorPayload('{"error":"nope","source":"provider","status":500}')
    expect(JSON.parse(payload)).toEqual({ error: 'nope' })
  })

  test('malformed JSON falls back to the raw string', () => {
    expect(normalizeErrorPayload('{not json')).toBe(
      JSON.stringify({ error: '{not json' }, null, 2),
    )
  })

  test('null / undefined become an empty error string, never crash', () => {
    expect(normalizeErrorPayload(null)).toBe(JSON.stringify({ error: '' }, null, 2))
    expect(normalizeErrorPayload(undefined)).toBe(JSON.stringify({ error: '' }, null, 2))
  })
  test('nested object error field is serialized, never "[object Object]"', () => {
    // Provider errors sometimes carry a JSON body under `error`; the payload
    // must round-trip that value so the user still sees what went wrong.
    const payload = normalizeErrorPayload({ error: { code: 429, msg: 'rate limit' } })
    const parsed = JSON.parse(payload)
    expect(typeof parsed.error).toBe('string')
    expect(parsed.error).not.toBe('[object Object]')
    expect(JSON.parse(parsed.error)).toEqual({ code: 429, msg: 'rate limit' })
  })
})

describe('mergeHydratedWithLocalErrors', () => {
  test('preserves an unpersisted transport error across a hydrate refresh', () => {
    const local = {
      id: 'local_err_1',
      role: 'assistant',
      content: '',
      error: JSON.stringify({ error: 'network unreachable' }, null, 2),
      errorSource: 'transport',
    }
    const hydrated = [{ id: 'srv_1', role: 'user', content: 'hi' }]
    const merged = mergeHydratedWithLocalErrors(hydrated, [...hydrated, local])
    expect(merged.at(-1)).toBe(local)
  })

  test('drops the local orphan once the server has persisted the error row', () => {
    // Same-id server-persisted row wins so the merge does not duplicate it.
    const local = {
      id: 'srv_err',
      role: 'assistant',
      content: '',
      error: JSON.stringify({ error: 'boom' }, null, 2),
      errorSource: 'server',
    }
    const hydrated = [
      { id: 'srv_err', role: 'assistant', content: '', error: 'x', errorSource: 'server' },
    ]
    const merged = mergeHydratedWithLocalErrors(hydrated, [local])
    expect(merged).toHaveLength(1)
    expect(merged[0].id).toBe('srv_err')
  })

  test('carries the immediately-preceding local user prompt alongside the error', () => {
    // Optimistic user + local-error assistant: when hydrate returns without
    // either (POST never landed), Retry needs the user row to walk back to.
    const user = { id: 'local_u', role: 'user', content: 'do it' }
    const err = {
      id: 'local_err',
      role: 'assistant',
      content: '',
      error: JSON.stringify({ error: 'oops' }, null, 2),
      errorSource: 'transport',
      retryPrompt: { content: 'do it' },
    }
    const hydrated = [{ id: 'srv_1', role: 'user', content: 'earlier' }]
    const merged = mergeHydratedWithLocalErrors(hydrated, [...hydrated, user, err])
    expect(merged.map((m) => m.id)).toEqual(['srv_1', 'local_u', 'local_err'])
  })
})
