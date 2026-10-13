// The SSE parser (sse.ts; vscode-extension §12.2): every shape the server's
// stream can take on the wire, chunked every way a network can chunk it.

import { describe, expect, it } from 'vitest'
import { SseParser, type Frame } from './sse'

const ev = (id: number, kind = 'workspace.state') => JSON.stringify({ id, kind, at: '2026-10-13T00:00:00Z', level: 'info', message: kind })

function all(chunks: string[]): Frame[] {
  const p = new SseParser()
  return chunks.flatMap((c) => p.feed(c))
}

/** Every way to cut `s` into two chunks, and one character at a time. */
function cuts(s: string): string[][] {
  const out: string[][] = [[s], [...s]]
  for (let i = 1; i < s.length; i++) out.push([s.slice(0, i), s.slice(i)])
  return out
}

describe('SseParser', () => {
  it('an unnamed event: data with its id, however the chunks fall', () => {
    const wire = `id: 7\ndata: ${ev(7)}\n\n`
    for (const c of cuts(wire)) {
      expect(all(c)).toEqual([{ kind: 'event', type: 'message', data: ev(7), lastEventId: '7' }])
    }
  })

  it('CRLF and lone CR line ends, a CRLF split across chunks counted once', () => {
    const crlf = `id: 1\r\ndata: a\r\n\r\nid: 2\r\ndata: b\r\n\r\n`
    for (const c of cuts(crlf)) {
      expect(all(c).map((f) => (f.kind === 'event' ? f.data : f.kind))).toEqual(['a', 'b'])
    }
    expect(all(['id: 3\rdata: c\r\r']).map((f) => f.kind === 'event' && f.data)).toEqual(['c'])
  })

  it('a heartbeat is a comment and dispatches nothing', () => {
    expect(all([': ping\n\n'])).toEqual([{ kind: 'comment', text: 'ping' }])
  })

  it('an id-only frame (an event this token may not see) moves the last event id and dispatches nothing', () => {
    const p = new SseParser()
    expect(p.feed('id: 9\n\n')).toEqual([{ kind: 'id', lastEventId: '9' }])
    expect(p.lastEventId).toBe('9')
    // Control: the next event carries on from it.
    expect(p.feed(`id: 10\ndata: ${ev(10)}\n\n`)).toEqual([{ kind: 'event', type: 'message', data: ev(10), lastEventId: '10' }])
  })

  it('a resync is a named event with its id', () => {
    expect(all(['id: 50\nevent: resync\ndata: {}\n\n'])).toEqual([{ kind: 'event', type: 'resync', data: '{}', lastEventId: '50' }])
  })

  it('a named frame without an id keeps the last id (the resources frame)', () => {
    expect(all(['id: 4\ndata: x\n\n', 'event: resources\ndata: {"round":1}\n\n'])[1]).toEqual(
      { kind: 'event', type: 'resources', data: '{"round":1}', lastEventId: '4' })
  })

  it('data split over two lines is joined by a newline; an unknown kind passes through as data', () => {
    expect(all(['data: {"id":1,\ndata: "kind":"brand.new"}\n\n'])).toEqual(
      [{ kind: 'event', type: 'message', data: '{"id":1,\n"kind":"brand.new"}', lastEventId: '' }])
  })

  it('one leading space is dropped and no more; retry, unknown fields and an id with a NUL are ignored', () => {
    expect(all(['data:  two\nretry: 5\nfoo: bar\nid: 1\u00002\n\n'])).toEqual(
      [{ kind: 'event', type: 'message', data: ' two', lastEventId: '' }])
  })

  it('a frame not yet ended dispatches nothing until its blank line', () => {
    const p = new SseParser()
    expect(p.feed('id: 1\ndata: x\n')).toEqual([])
    expect(p.feed('\n')).toHaveLength(1)
  })
})
