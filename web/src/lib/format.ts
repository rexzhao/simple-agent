export function relativeTime(value: string): string {
  const timestamp = new Date(value).getTime()
  if (!Number.isFinite(timestamp)) return 'just now'
  const seconds = Math.max(0, Math.round((Date.now() - timestamp) / 1000))
  if (seconds < 60) return 'just now'
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h ago`
  if (seconds < 604800) return `${Math.floor(seconds / 86400)}d ago`
  return new Date(value).toLocaleDateString('en-US')
}

export function formatTokenCount(tokens: number): string {
	if (tokens >= 1_000_000) return `${(tokens / 1_000_000).toFixed(1).replace(/\.0$/, '')}M`
	if (tokens >= 1_000) return `${(tokens / 1_000).toFixed(1).replace(/\.0$/, '')}K`
	return Math.max(0, tokens).toLocaleString()
}

export function formatCost(amount: number, currency = 'USD'): string {
  if (!Number.isFinite(amount)) return '—'
  const code = currency.trim().toUpperCase() || 'USD'
  try {
    return new Intl.NumberFormat(undefined, {
      style: 'currency',
      currency: code,
      minimumFractionDigits: amount === 0 ? 2 : 4,
      maximumFractionDigits: 6,
    }).format(amount)
  } catch {
    return `${code} ${amount.toFixed(6).replace(/0+$/, '').replace(/\.$/, '')}`
  }
}

export function formatTime(value: string): string {
  const date = new Date(value)
  return Number.isFinite(date.getTime()) ? date.toLocaleTimeString('en-US', { hour: '2-digit', minute: '2-digit' }) : ''
}

export function formatCompletionTime(value: string): string {
  const date = new Date(value)
  if (!Number.isFinite(date.getTime())) return ''
  return date.toLocaleString('en-US', {
    year: 'numeric',
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
  })
}

export function formatDuration(durationMS: number): string {
  if (!Number.isFinite(durationMS) || durationMS < 0) return ''
  if (durationMS < 1000) return `${Math.round(durationMS)}ms`

  const totalSeconds = durationMS / 1000
  if (totalSeconds < 60) {
    const seconds = totalSeconds < 10 ? totalSeconds.toFixed(1).replace(/\.0$/, '') : Math.round(totalSeconds).toString()
    return `${seconds}s`
  }

  const roundedSeconds = Math.round(totalSeconds)
  const totalMinutes = Math.floor(roundedSeconds / 60)
  if (totalMinutes < 60) {
    const minutes = Math.floor(roundedSeconds / 60)
    const seconds = roundedSeconds % 60
    return `${minutes}m${seconds > 0 ? ` ${seconds}s` : ''}`
  }

  const hours = Math.floor(totalMinutes / 60)
  const minutes = totalMinutes % 60
  return `${hours}h${minutes > 0 ? ` ${minutes}m` : ''}`
}

export function unicodeCodePointLength(value: string): number {
  // JavaScript string length counts UTF-16 code units; the UI displays Unicode characters.
  return Array.from(value).length
}

export function errorMessage(reason: unknown): string {
  return reason instanceof Error ? reason.message : 'An unknown error occurred'
}

export function blobAsDataURL(blob: Blob): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader()
    reader.onerror = () => reject(reader.error ?? new Error('could not read image attachment'))
    reader.onload = () => typeof reader.result === 'string' ? resolve(reader.result) : reject(new Error('could not read image attachment'))
    reader.readAsDataURL(blob)
  })
}

export function formatBytes(bytes: number): string {
  if (bytes >= 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(1).replace(/\.0$/, '')} MiB`
  return `${Math.max(0, Math.ceil(bytes / 1024))} KiB`
}

export async function copyText(text: string): Promise<void> {
	if (navigator.clipboard?.writeText) {
		await navigator.clipboard.writeText(text)
		return
	}
	const textarea = document.createElement('textarea')
	textarea.value = text
	textarea.style.position = 'fixed'
	textarea.style.opacity = '0'
	document.body.appendChild(textarea)
	textarea.select()
	try {
		if (!document.execCommand('copy')) throw new Error('copy command was rejected')
	} finally {
		textarea.remove()
	}
}

export function parseJSONRecord(value: string, label: string): Record<string, unknown> {
  try {
    const parsed: unknown = JSON.parse(value || '{}')
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) throw new Error('must be a JSON object')
    return parsed as Record<string, unknown>
  } catch (reason) {
    throw new Error(`${label} is invalid: ${errorMessage(reason)}`)
  }
}

export function prettyJSON(value: Record<string, unknown>): string {
  return JSON.stringify(value, null, 2)
}
