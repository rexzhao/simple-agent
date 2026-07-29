// Presentation helpers for the session orchestration tools (session_*):
// one-line summaries for collapsed tool rows, folded-group phrases, and
// pretty-printing for their JSON arguments and results. Keeping this logic
// pure (no React) lets the timeline rendering stay declarative and lets the
// summaries be unit-tested without a DOM.

const sessionToolNames = new Set([
	'session_models',
	'session_start',
	'session_search',
	'session_get',
	'session_history',
	'session_send',
	'session_wait',
	'session_stop',
])

export function isSessionToolName(name: string): boolean {
	return sessionToolNames.has(name)
}

// sessionToolTarget is the collapsed-row summary of a session tool call. It
// names the target session (display name when the UI knows it, raw id
// otherwise) plus the call's key argument, so a mistaken call — say, two
// conflicting history cursors — is visible without expanding the row.
export function sessionToolTarget(name: string, args: Record<string, unknown>, sessionNames?: Record<string, string>): string {
	const label = sessionLabel(stringValue(args, 'session_id'), sessionNames)
	switch (name) {
		case 'session_start': {
			const sessionName = stringValue(args, 'name')
			const model = stringValue(args, 'model')
			const prompt = inlineSnippet(stringValue(args, 'prompt'))
			const head = sessionName ? `"${sessionName}"` : prompt
			return [head, model].filter(Boolean).join(' · ')
		}
		case 'session_search': {
			const regex = stringValue(args, 'name_regex')
			const archived = args.include_archived === true ? 'archived' : ''
			return [regex && `/${regex}/`, archived].filter(Boolean).join(' · ')
		}
		case 'session_get':
		case 'session_wait':
		case 'session_stop':
			return label
		case 'session_history': {
			// Current contract is cursor + direction; the retired
			// before_seq/after_seq arguments may still appear in in-flight or
			// historical calls and stay visible so conflicting calls remain
			// obvious from the collapsed row.
			const cursor = numberValue(args, 'cursor')
			const direction = stringValue(args, 'direction')
			const before = numberValue(args, 'before_seq')
			const after = numberValue(args, 'after_seq')
			const parts: string[] = []
			if (cursor !== undefined) parts.push(direction === 'before' || direction === 'after' ? `${direction} ${cursor}` : `cursor ${cursor}`)
			if (before !== undefined) parts.push(`before ${before}`)
			if (after !== undefined) parts.push(`after ${after}`)
			return [label, ...parts].filter(Boolean).join(' · ')
		}
		case 'session_send': {
			const mode = stringValue(args, 'mode')
			const message = inlineSnippet(stringValue(args, 'message'))
			return [label, mode, message && `"${message}"`].filter(Boolean).join(' · ')
		}
		default:
			return ''
	}
}

// sessionToolPhrase summarizes session tool calls inside a folded tool group,
// mirroring the phrasing style of the file/shell tools.
export function sessionToolPhrase(name: string, count: number): string {
	const plural = count === 1 ? '' : 's'
	switch (name) {
		case 'session_models':
			return 'Listed session models'
		case 'session_start':
			return `Started ${count} session${plural}`
		case 'session_search':
			return count === 1 ? 'Searched sessions' : `Searched sessions ×${count}`
		case 'session_get':
			return count === 1 ? 'Inspected a session' : `Inspected sessions ×${count}`
		case 'session_history':
			return count === 1 ? 'Read session history' : `Read session history ×${count}`
		case 'session_send':
			return `Sent ${count} session message${plural}`
		case 'session_wait':
			return count === 1 ? 'Waited on a session' : `Waited on sessions ×${count}`
		case 'session_stop':
			return `Stopped ${count} session${plural}`
		default:
			return count > 1 ? `${name} ×${count}` : name
	}
}

// prettyJSONText formats JSON payloads for the expanded tool details, leaving
// non-JSON text untouched.
export function prettyJSONText(text: string): string {
	const trimmed = text.trim()
	if (!trimmed) return text
	try {
		return JSON.stringify(JSON.parse(trimmed), null, 2)
	} catch {
		return text
	}
}

function sessionLabel(id: string, sessionNames?: Record<string, string>): string {
	if (!id) return ''
	return sessionNames?.[id] || id
}

function stringValue(args: Record<string, unknown>, key: string): string {
	const value = args[key]
	return typeof value === 'string' ? value : ''
}

function numberValue(args: Record<string, unknown>, key: string): number | undefined {
	const value = args[key]
	return typeof value === 'number' ? value : undefined
}

const snippetMaxLength = 80

function inlineSnippet(text: string): string {
	const collapsed = text.replace(/\s+/g, ' ').trim()
	if (collapsed.length <= snippetMaxLength) return collapsed
	return `${collapsed.slice(0, snippetMaxLength)}…`
}
