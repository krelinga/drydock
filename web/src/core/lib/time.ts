// The few dozen bytes of relative-time logic §3.1 says to write rather than
// import.

const UNITS: Array<[limit: number, seconds: number, name: string]> = [
  [60, 1, 'second'],
  [3600, 60, 'minute'],
  [86400, 3600, 'hour'],
  [86400 * 30, 86400, 'day'],
  [86400 * 365, 86400 * 30, 'month'],
  [Infinity, 86400 * 365, 'year'],
]

/** `just now`, `5 minutes ago`, `in 3 days`. Invalid input renders as `unknown`. */
export function relativeTime(iso: string, now: number = Date.now()): string {
  const t = Date.parse(iso)
  if (Number.isNaN(t)) return 'unknown'
  const delta = Math.round((t - now) / 1000)
  const abs = Math.abs(delta)
  if (abs < 45) return 'just now'
  for (const [limit, size, name] of UNITS) {
    if (abs < limit) {
      const n = Math.round(abs / size)
      const unit = `${n} ${name}${n === 1 ? '' : 's'}`
      return delta < 0 ? `${unit} ago` : `in ${unit}`
    }
  }
  return 'unknown'
}
