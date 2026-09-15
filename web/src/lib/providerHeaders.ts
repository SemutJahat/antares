const headerName = /^[!#$%&'*+.^_\x60|~0-9A-Za-z-]+$/

export function parseProviderHeaders(text: string): Record<string, string> {
  const headers = Object.create(null) as Record<string, string>
  const seen = new Set<string>()

  for (const rawLine of text.replace(/\r\n?/g, '\n').split('\n')) {
    if (!rawLine.trim()) continue
    const firstEquals = rawLine.indexOf('=')
    if (firstEquals === -1) {
      if (rawLine.trimStart().startsWith('#')) continue
      throw new Error('invalid_header_entries')
    }

    const rawName = rawLine.slice(0, firstEquals)
    const rawValue = rawLine.slice(firstEquals + 1)
    if (/[\x00-\x08\x0B-\x1F\x7F]/.test(rawValue)) throw new Error('invalid_header_entries')

    const name = rawName.replace(/^[ \t]+|[ \t]+$/g, '')
    const value = rawValue.replace(/^[ \t]+|[ \t]+$/g, '')
    if (!headerName.test(name)) throw new Error('invalid_header_entries')

    const folded = name.toLowerCase()
    if (seen.has(folded)) throw new Error('invalid_header_entries')
    seen.add(folded)
    headers[name] = value
  }

  return headers
}

export function formatProviderHeaders(headers?: Record<string, string> | null): string {
  if (!headers) return ''
  return Object.keys(headers)
    .sort((a, b) => a.localeCompare(b))
    .map((name) => `${name}=${headers[name]}`)
    .join('\n')
}
