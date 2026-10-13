// core's in-flight marks on their own clock (vscode-extension §5.2). The web
// UI's stream store delegates to them, and its specs (stores/stream.spec.ts,
// stores/inflightResync.spec.ts, stores/jobSettle.spec.ts) are the proof that
// nothing moved but the code; these pin the class itself, as the extension
// will hold it, with no store and no global timer.

import { describe, expect, it } from 'vitest'
import { emptyEntities, reduceAll, type Entities } from './reducer'
import { Marks, SLOW_AFTER_MS, ownState, type Clock, type InFlight } from './marks'
import { CLONE_OK, WS, stateEvent } from './reducer.fixtures'
import { OVER, overIn, settlesStop } from './settle'

class FakeClock implements Clock {
  t = 1_000
  timers: Array<{ at: number; fn: () => void; live: boolean }> = []
  now() {
    return this.t
  }
  after(ms: number, fn: () => void) {
    const timer = { at: this.t + ms, fn, live: true }
    this.timers.push(timer)
    return () => { timer.live = false }
  }
  advance(ms: number) {
    this.t += ms
    for (const timer of this.timers) {
      if (timer.live && timer.at <= this.t) {
        timer.live = false
        timer.fn()
      }
    }
  }
  pending() {
    return this.timers.filter((t) => t.live).length
  }
}

const KEY = `workspace:${WS}:stop`
const running = (): Entities => reduceAll(emptyEntities(), CLONE_OK.map((event) => ({ type: 'event' as const, event })))
const stopped = (e: Entities): Entities => reduceAll(e, [{ type: 'event', event: stateEvent(200, WS, 'stopped', { from: 'running' }) }])

function setup() {
  const clock = new FakeClock()
  const seen: Array<Readonly<Record<string, InFlight>>> = []
  const marks = new Marks(clock, ownState((next) => seen.push(next)))
  return { clock, marks, seen }
}

describe('Marks', () => {
  it('a mark ends on its settling event and not on any other', () => {
    const { marks } = setup()
    marks.begin(KEY, settlesStop(WS))
    marks.onEvent(stateEvent(150, WS, 'running', { from: 'running' }))
    expect(marks.has(KEY)).toBe(true)
    marks.onEvent(stateEvent(151, WS, 'stopped', { from: 'running' }))
    expect(marks.has(KEY)).toBe(false)
  })

  it('says "no response yet" after SLOW_AFTER_MS on its clock, and an end cancels the timer', () => {
    const { clock, marks, seen } = setup()
    marks.begin(KEY, () => false)
    expect(marks.inFlight[KEY]).toEqual({ since: 1_000, slow: false })
    clock.advance(SLOW_AFTER_MS - 1)
    expect(marks.inFlight[KEY]!.slow).toBe(false)
    clock.advance(1)
    expect(marks.inFlight[KEY]!.slow).toBe(true)
    // Each change was a new record, told to the owner.
    expect(seen.length).toBe(2)
    expect(seen[0]).not.toBe(seen[1])

    marks.begin('other', () => false)
    marks.end('other')
    expect(clock.pending()).toBe(0)
  })

  it('a snapshot ends a mark only once its request was accepted before the snapshot was requested', () => {
    const { marks } = setup()
    const over = stopped(running())
    marks.begin(KEY, () => false, overIn(WS, OVER.stop))

    // Requested before the 2xx: it cannot tell "not begun" from "over".
    const early = marks.snapshotTag()
    marks.accepted(KEY)
    marks.onSnapshot(over, early)
    expect(marks.has(KEY)).toBe(true)

    // A snapshot showing the action under way keeps it.
    const late = marks.snapshotTag()
    marks.onSnapshot(running(), late)
    expect(marks.has(KEY)).toBe(true)

    // Requested after the 2xx and showing it over: the control, it ends.
    marks.onSnapshot(over, late)
    expect(marks.has(KEY)).toBe(false)
  })

  it('a mark begun with no resolver is never ended by a snapshot', () => {
    const { marks } = setup()
    marks.begin(KEY, () => false)
    marks.accepted(KEY)
    marks.onSnapshot(stopped(running()), marks.snapshotTag())
    expect(marks.has(KEY)).toBe(true)
  })

  it('accepted is ignored for a key not in flight', () => {
    const { marks } = setup()
    marks.accepted(KEY)
    marks.begin(KEY, () => false, () => true)
    marks.onSnapshot(running(), marks.snapshotTag())
    expect(marks.has(KEY)).toBe(true)
  })

  it('clear forgets settlers and timers but leaves the record to its owner', () => {
    const { clock, marks } = setup()
    marks.begin(KEY, () => true)
    marks.clear()
    marks.onEvent(stateEvent(151, WS, 'stopped', { from: 'running' }))
    expect(marks.has(KEY)).toBe(true)
    expect(clock.pending()).toBe(0)
  })
})
