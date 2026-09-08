export function formatTime(iso: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' })
}

export function formatDateTime(iso: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return d.toLocaleString()
}

export function shortId(id: string): string {
  return id.length > 12 ? `${id.slice(0, 6)}…${id.slice(-4)}` : id
}

let counter = 0

// clientOrderId is the idempotency key of one order: unique per browser
// session, well under the API's 64-character limit.
export function clientOrderId(): string {
  counter += 1
  return `web-${Date.now().toString(36)}-${counter}-${Math.random().toString(36).slice(2, 8)}`
}

export function idempotencyKey(): string {
  return `web-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`
}
