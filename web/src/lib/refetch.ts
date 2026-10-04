import { onMounted, onUnmounted } from 'vue'
import { useStreamStore, type Refetcher } from '../stores/stream'

/**
 * Registers what the current view must refetch when the stream reopens, sends
 * a `resync`, or carries an event `when` accepts (frontend §4.3's backstop).
 * Registered while the view is mounted, so "what the current route needs" is
 * whatever is on screen, with no route table to keep in step.
 */
export function useStreamRefetch(r: Refetcher): void {
  const stream = useStreamStore()
  let off: (() => void) | null = null
  onMounted(() => {
    off = stream.addRefetcher(r)
  })
  onUnmounted(() => {
    off?.()
    off = null
  })
}
