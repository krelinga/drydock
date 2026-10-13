// The event stream's wire format, parsed (vscode-extension §5.3), as the
// HTML standard's event-stream interpretation says, since that is what the
// server writes for (internal/api/events_routes.go):
//
//   - Lines end in CRLF, LF or CR, and a CR at the end of one chunk may be
//     followed by the LF of the next, which is not a second line end.
//   - `data:` lines accumulate, joined by LF; `event:` names the frame;
//     `id:` sets the last event id unless it holds a NUL; `retry:` and
//     unknown fields are ignored; a line starting with `:` is a comment (the
//     server's `: ping` heartbeat). One leading space after the colon is
//     dropped.
//   - A blank line dispatches. The last event id moves even when the frame
//     has no data: that is how the server sends an event this credential may
//     not see (§4.1), as its id alone, so the position still moves past it
//     and a reconnect does not resync for it.

export type Frame =
  /** An event to dispatch: unnamed (`message`, a domain event's JSON) or named (`resync`, `resources`). */
  | { kind: 'event'; type: string; data: string; lastEventId: string }
  /** A frame with no data: nothing to dispatch, but the last event id is now `lastEventId`. */
  | { kind: 'id'; lastEventId: string }
  /** A comment line: a heartbeat. */
  | { kind: 'comment'; text: string }

export class SseParser {
  #buf = ''
  #skipLF = false
  #data: string[] = []
  #type = ''
  #idBuf = ''
  #sawId = false
  #lastEventId = ''

  /** The last event id the stream has set (the standard's "last event ID string"). */
  get lastEventId(): string {
    return this.#lastEventId
  }

  /** Feeds a chunk of decoded text; returns every frame it completes, in order. */
  feed(chunk: string): Frame[] {
    const out: Frame[] = []
    let s = chunk
    if (this.#skipLF) {
      this.#skipLF = false
      if (s.startsWith('\n')) s = s.slice(1)
    }
    this.#buf += s
    for (;;) {
      const m = /\r\n|\r|\n/.exec(this.#buf)
      if (m === null) break
      // A CR at the very end may be half of a CRLF split across chunks: take
      // the line now, and drop an LF that opens the next chunk.
      const line = this.#buf.slice(0, m.index)
      if (m[0] === '\r' && m.index === this.#buf.length - 1) this.#skipLF = true
      this.#buf = this.#buf.slice(m.index + m[0].length)
      this.#line(line, out)
    }
    return out
  }

  #line(line: string, out: Frame[]): void {
    if (line === '') {
      this.#dispatch(out)
      return
    }
    if (line.startsWith(':')) {
      out.push({ kind: 'comment', text: line.slice(1).replace(/^ /, '') })
      return
    }
    const colon = line.indexOf(':')
    const field = colon === -1 ? line : line.slice(0, colon)
    const value = colon === -1 ? '' : line.slice(colon + 1).replace(/^ /, '')
    switch (field) {
      case 'data':
        this.#data.push(value)
        break
      case 'event':
        this.#type = value
        break
      case 'id':
        if (!value.includes('\u0000')) {
          this.#idBuf = value
          this.#sawId = true
        }
        break
      default:
        break
    }
  }

  #dispatch(out: Frame[]): void {
    // The standard sets the last event id from its buffer at every dispatch;
    // the buffer keeps its value across frames, so a frame without `id:`
    // keeps the last one.
    if (this.#sawId) this.#lastEventId = this.#idBuf
    const data = this.#data
    const type = this.#type
    this.#data = []
    this.#type = ''
    if (data.length === 0) {
      if (this.#sawId) out.push({ kind: 'id', lastEventId: this.#lastEventId })
      this.#sawId = false
      return
    }
    this.#sawId = false
    out.push({ kind: 'event', type: type === '' ? 'message' : type, data: data.join('\n'), lastEventId: this.#lastEventId })
  }
}
