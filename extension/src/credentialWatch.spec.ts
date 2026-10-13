// A credential change and the stream (credentialWatch.ts; vscode-extension
// §4.2, §5.5): a sign-in with another token closes the old stream once, and
// the late notification of that same change leaves alone the stream the next
// command opened with the new token.

import { describe, expect, it } from 'vitest'
import { stateEvent, wsView } from '../../web/src/core/reducer.fixtures'
import type { WorkspaceView } from '../../web/src/core/types'
import { CredentialWatch } from './credentialWatch'
import { CONNECT, clone } from './flows'
import { PASTE_TOKEN, signIn } from './session'
import { LINK, NEW_WS, flowWorld } from '../test/fakeDrydock'
import { ORIGIN, TOKEN2, flush } from '../test/fakes'

describe('CredentialWatch', () => {
  it('a sign-in applied by its own command closes the old stream; the late notification does not close the new one', async () => {
    const w = flowWorld()
    const watch = new CredentialWatch(w)
    expect(await watch.check()).toBe(true)
    const first = w.live.hold()
    await flush()
    const releaseFirst = await first
    expect(w.server.streams).toBe(1)

    // Sign In stores the new token and applies the change before it returns.
    w.secrets.value = JSON.stringify({ origin: ORIGIN, token: TOKEN2 })
    expect(await watch.check()).toBe(true)
    expect(w.live.streaming).toBe(false)
    expect(w.live.endedBy).toBe('signed_in_again')
    releaseFirst()

    // The next command opens a stream with the new token…
    const next = w.live.hold()
    await flush()
    const release = await next
    expect(w.server.streams).toBe(2)
    expect(w.server.transport.streams.at(-1)!.headers.Authorization).toBe(`Bearer ${TOKEN2}`)

    // …and SecretStorage's onDidChange for that same change arrives late:
    // twice, for the clear and the write. The stream stays open.
    expect(await watch.check()).toBe(true)
    expect(await watch.check()).toBe(true)
    expect(w.live.streaming).toBe(true)
    expect(w.live.endedBy).toBe(null)

    // Control: a change this window has not seen (another window signs in
    // with a third token, then out) closes it, each with its reason.
    w.secrets.value = JSON.stringify({ origin: ORIGIN, token: 'drydock_' + 'Q'.repeat(43) })
    expect(await watch.check()).toBe(true)
    expect(w.live.streaming).toBe(false)
    expect(w.live.endedBy).toBe('signed_in_again')
    const again = w.live.hold()
    await flush()
    const releaseAgain = await again
    expect(w.server.streams).toBe(3)
    w.secrets.value = undefined
    expect(await watch.check()).toBe(false)
    expect(w.live.streaming).toBe(false)
    expect(w.live.endedBy).toBe('signed_out')
    releaseAgain()
    release()
  })

  it('checks run in order: a read begun first is applied first', async () => {
    const w = flowWorld()
    const reads: Array<(v: string | undefined) => void> = []
    const watch = new CredentialWatch({
      live: w.live,
      credentials: { read: () => new Promise((r) => reads.push((v) => r(v === undefined ? null : JSON.parse(v)))) },
    })
    const a = watch.check()
    const b = watch.check()
    await flush()
    // The second read is not even asked until the first is applied.
    expect(reads).toHaveLength(1)
    reads[0]!(undefined)
    expect(await a).toBe(false)
    await flush()
    expect(reads).toHaveLength(2)
    reads[1]!(JSON.stringify({ origin: ORIGIN, token: TOKEN2 }))
    expect(await b).toBe(true)
  })
})

describe('a sign-in, then a clone while its notification is still open', () => {
  // The notification's promise settles only when the operator closes it, so
  // the clone runs while Sign In has not returned; SecretStorage's late
  // onDidChange then arrives. The clone must still be followed to running.
  async function signInThenClone(notificationCloses: boolean) {
    const w = flowWorld()
    const watch = new CredentialWatch(w)
    await watch.check()
    const hold = w.live.hold()
    await flush()
    ;(await hold)()
    expect(w.live.streaming).toBe(true)
    w.prompter.message = async function (level, text, actions = []) {
      this.shown.push({ level, text, actions })
      if (actions.includes(PASTE_TOKEN)) return PASTE_TOKEN
      if (text.startsWith('Signed in to Drydock')) return notificationCloses ? undefined : new Promise<undefined>(() => {})
      return text.includes(' is running.') ? CONNECT : undefined
    }
    w.prompter.typed = [TOKEN2]
    void signIn({ ...w, applied: () => watch.check() })
    await flush()
    expect(w.prompter.shown.some((s) => s.text.startsWith('Signed in to Drydock'))).toBe(true)
    expect(JSON.parse(w.secrets.value!).token).toBe(TOKEN2)

    w.server.workspaces.push({ ...wsView({ id: NEW_WS, repository_id: 2, full_name: 'krelinga/homelab', state: 'running' }), vscode: { configured: true, url: LINK } } as WorkspaceView)
    const done = clone(w, 2)
    await flush()
    // The sign-in's onDidChange, late: once for the clear, once for the write.
    await watch.check()
    await watch.check()
    await flush()
    w.server.emit(stateEvent(203, NEW_WS, 'running', { from: 'building' }))
    for (let i = 0; i < 3 && w.opener.folders.length === 0; i++) {
      w.clock.advance(1_000)
      await flush()
    }
    await done
    expect(w.server.transport.streams.at(-1)!.headers.Authorization).toBe(`Bearer ${TOKEN2}`)
    expect(w.prompter.shown.map((s) => s.text).filter((t) => t.startsWith('Stopped following'))).toEqual([])
    expect(w.opener.folders).toHaveLength(1)
  }

  it('the notification never closed: the clone is followed to running on the new token', () => signInThenClone(false))
  it('the notification closed at once (the control)', () => signInThenClone(true))
})
