import { describe, expect, test } from 'bun:test'
import { formatProviderHeaders, parseProviderHeaders } from './providerHeaders.ts'

describe('provider header parsing', () => {
  test('retains first-equals values and empty values', () => {
    expect(parseProviderHeaders('X-Tenant=team=a=b\nX-Empty=')).toEqual({
      'X-Tenant': 'team=a=b',
      'X-Empty': '',
    })
  })

  test('accepts blank text and comments', () => {
    expect(parseProviderHeaders(' \n # comment\r\n')).toEqual({})
  })

  test('rejects duplicate casing and malformed entries', () => {
    for (const input of [
      'X-Test=one\nx-test=two',
      'Bad Header=value',
      'X-Test=bad\u0001value',
      'X-Test=value\r',
      'X-Test=value\rY-Test=other',
    ]) {
      expect(() => parseProviderHeaders(input)).toThrow('invalid_header_entries')
    }
  })

  test('accepts prototype-like names without a prototype-bearing result', () => {
    const headers = parseProviderHeaders('__proto__=value\nconstructor=other')
    expect(Object.getPrototypeOf(headers)).toBeNull()
    expect(headers.__proto__).toBe('value')
    expect(headers.constructor).toBe('other')
  })

  test('formats stored maps without losing values', () => {
    const stored = { 'X-Tenant': 'team=a=b', Authorization: 'Bearer token' }
    const text = formatProviderHeaders(stored)
    expect(text).toBe('Authorization=Bearer token\nX-Tenant=team=a=b')
    expect(parseProviderHeaders(text)).toEqual(stored)
  })
})
