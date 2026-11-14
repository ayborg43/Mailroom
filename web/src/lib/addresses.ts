// Parsing helpers for the compose recipient fields. They only decide how to
// split and display what the user typed — the server still parses and
// validates every address with net/mail before anything is sent.

/** Splits "a@x.com, "Doe, Jo" <jo@y.com>" on top-level commas/semicolons/newlines,
 *  leaving commas inside quotes or <angle brackets> alone. */
export function splitAddresses(input: string): string[] {
  const out: string[] = []
  let cur = ''
  let inQuotes = false
  let inAngle = false
  for (let i = 0; i < input.length; i++) {
    const ch = input[i]
    if (inQuotes) {
      cur += ch
      if (ch === '\\' && i + 1 < input.length) cur += input[++i]
      else if (ch === '"') inQuotes = false
      continue
    }
    if (ch === '"') {
      inQuotes = true
      cur += ch
    } else if (ch === '<') {
      inAngle = true
      cur += ch
    } else if (ch === '>') {
      inAngle = false
      cur += ch
    } else if (!inAngle && (ch === ',' || ch === ';' || ch === '\n' || ch === '\r')) {
      out.push(cur)
      cur = ''
    } else {
      cur += ch
    }
  }
  out.push(cur)
  return out.map((s) => s.trim()).filter(Boolean)
}

const BARE = /^[^\s@<>",;]+@[^\s@<>",;]+\.[^\s@<>",;]+$/
const NAMED = /^(?:"(?:[^"\\]|\\.)*"|[^<>"]*)\s*<([^\s@<>]+@[^\s@<>]+\.[^\s@<>]+)>$/

/** True if `addr` looks like `a@b.c` or `Name <a@b.c>`. */
export function isValidAddress(addr: string): boolean {
  const s = addr.trim()
  return BARE.test(s) || NAMED.test(s)
}

/** What a chip shows: the display name if there is one, else the address. */
export function displayLabel(addr: string): string {
  const m = addr.trim().match(/^(?:"((?:[^"\\]|\\.)*)"|([^<>"]*?))\s*<([^>]+)>$/)
  if (!m) return addr.trim()
  const name = (m[1] ?? m[2] ?? '').replace(/\\(.)/g, '$1').trim()
  return name || m[3]
}

/** Adds addresses to a list, skipping case-insensitive duplicates. */
export function addAddresses(existing: string[], incoming: string[]): string[] {
  const seen = new Set(existing.map((a) => a.toLowerCase()))
  const next = [...existing]
  for (const a of incoming) {
    const key = a.toLowerCase()
    if (!seen.has(key)) {
      seen.add(key)
      next.push(a)
    }
  }
  return next
}

export function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`
  const units = ['KB', 'MB', 'GB']
  let v = n / 1024
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v.toFixed(1)} ${units[i]}`
}
