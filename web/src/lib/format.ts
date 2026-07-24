export function relativeTime(value: string): string {
  const timestamp = new Date(value).getTime()
  if (!Number.isFinite(timestamp)) return '刚刚'
  const seconds = Math.max(0, Math.round((Date.now() - timestamp) / 1000))
  if (seconds < 60) return '刚刚'
  if (seconds < 3600) return `${Math.floor(seconds / 60)} 分钟前`
  if (seconds < 86400) return `${Math.floor(seconds / 3600)} 小时前`
  if (seconds < 604800) return `${Math.floor(seconds / 86400)} 天前`
  return new Date(value).toLocaleDateString('zh-CN')
}

export function formatTokenCount(tokens: number): string {
	if (tokens >= 1_000_000) return `${(tokens / 1_000_000).toFixed(1).replace(/\.0$/, '')}M`
	if (tokens >= 1_000) return `${(tokens / 1_000).toFixed(1).replace(/\.0$/, '')}K`
	return Math.max(0, tokens).toLocaleString()
}

export function formatTime(value: string): string {
  const date = new Date(value)
  return Number.isFinite(date.getTime()) ? date.toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' }) : ''
}

export function errorMessage(reason: unknown): string {
  return reason instanceof Error ? reason.message : '发生未知错误'
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
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) throw new Error('必须是 JSON 对象')
    return parsed as Record<string, unknown>
  } catch (reason) {
    throw new Error(`${label}格式错误：${errorMessage(reason)}`)
  }
}

export function prettyJSON(value: Record<string, unknown>): string {
  return JSON.stringify(value, null, 2)
}
