export type AnsiColor =
	| 'black'
	| 'red'
	| 'green'
	| 'yellow'
	| 'blue'
	| 'magenta'
	| 'cyan'
	| 'white'
	| 'bright-black'
	| 'bright-red'
	| 'bright-green'
	| 'bright-yellow'
	| 'bright-blue'
	| 'bright-magenta'
	| 'bright-cyan'
	| 'bright-white'

export type AnsiStyle = {
	foreground?: AnsiColor
	background?: AnsiColor
	bold?: boolean
	dim?: boolean
	italic?: boolean
	underline?: boolean
}

export type AnsiToken = {
	text: string
	style: AnsiStyle
}

const foregroundColors: Record<number, AnsiColor> = {
	30: 'black', 31: 'red', 32: 'green', 33: 'yellow', 34: 'blue', 35: 'magenta', 36: 'cyan', 37: 'white',
	90: 'bright-black', 91: 'bright-red', 92: 'bright-green', 93: 'bright-yellow', 94: 'bright-blue', 95: 'bright-magenta', 96: 'bright-cyan', 97: 'bright-white',
}
const backgroundColors: Record<number, AnsiColor> = {
	40: 'black', 41: 'red', 42: 'green', 43: 'yellow', 44: 'blue', 45: 'magenta', 46: 'cyan', 47: 'white',
	100: 'bright-black', 101: 'bright-red', 102: 'bright-green', 103: 'bright-yellow', 104: 'bright-blue', 105: 'bright-magenta', 106: 'bright-cyan', 107: 'bright-white',
}

/**
 * Parse the small, display-only subset of ANSI used by shell output.
 *
 * CSI sequences other than SGR and OSC sequences are consumed and ignored;
 * they are never passed to the DOM. This is intentionally a style parser,
 * not a terminal emulator.
 */
export function parseAnsi(text: string): AnsiToken[] {
	const tokens: AnsiToken[] = []
	let style: AnsiStyle = {}
	let plain = ''

	const flush = () => {
		if (!plain) return
		const previous = tokens[tokens.length - 1]
		if (previous && sameStyle(previous.style, style)) {
			previous.text += plain
		} else {
			tokens.push({ text: plain, style: { ...style } })
		}
		plain = ''
	}

	for (let index = 0; index < text.length;) {
		if (text.charCodeAt(index) === 0x1b) {
			const escape = readEscape(text, index)
			if (escape) {
				flush()
				if (escape.final === 'm') style = applySGR(style, escape.body)
				index = escape.end
				continue
			}
			// A bare ESC is a control character, not displayable shell text.
			index++
			continue
		}

		const code = text.charCodeAt(index)
		// Preserve whitespace that matters to copied/readable output, but do not
		// render other C0 controls, DEL, or C1 controls as shell text. C1
		// sequences are not interpreted; filtering the control bytes prevents
		// them from reaching the DOM or plain-text extraction.
		if ((code < 0x20 && code !== 0x09 && code !== 0x0a && code !== 0x0c && code !== 0x0d) || (code >= 0x7f && code <= 0x9f)) {
			index++
			continue
		}
		plain += text[index]
		index++
	}
	flush()
	return tokens
}

/** Return shell output as readable text without ANSI/control sequences. */
export function ansiToPlainText(text: string): string {
	return parseAnsi(text).map((token) => token.text).join('')
}

/** Convert a parsed style to class names from the fixed ANSI stylesheet. */
export function ansiStyleClass(style: AnsiStyle): string {
	const classes = ['ansi-output-token']
	if (style.foreground) classes.push(`ansi-fg-${style.foreground}`)
	if (style.background) classes.push(`ansi-bg-${style.background}`)
	if (style.bold) classes.push('ansi-bold')
	if (style.dim) classes.push('ansi-dim')
	if (style.italic) classes.push('ansi-italic')
	if (style.underline) classes.push('ansi-underline')
	return classes.join(' ')
}

type EscapeSequence = { end: number; final?: string; body: string }

function readEscape(text: string, start: number): EscapeSequence | undefined {
	if (text[start + 1] === '[') return readCSI(text, start)
	if (text[start + 1] === ']') return readOSC(text, start)
	return undefined
}

function readCSI(text: string, start: number): EscapeSequence | undefined {
	for (let index = start + 2; index < text.length; index++) {
		const code = text.charCodeAt(index)
		if (code >= 0x40 && code <= 0x7e) {
			return { end: index + 1, final: text[index], body: text.slice(start + 2, index) }
		}
	}
	// Keep a truncated sequence's printable tail as ordinary text. It cannot
	// execute in the DOM, and this avoids dropping the end of a long result.
	return undefined
}

function readOSC(text: string, start: number): EscapeSequence {
	for (let index = start + 2; index < text.length; index++) {
		const code = text.charCodeAt(index)
		if (code === 0x07) return { end: index + 1, body: '' }
		if (code === 0x1b && text[index + 1] === '\\') return { end: index + 2, body: '' }
	}
	// Ignore an unterminated OSC through the end of this result.
	return { end: text.length, body: '' }
}

function applySGR(current: AnsiStyle, body: string): AnsiStyle {
	const parts = body === '' ? ['0'] : body.split(';')
	const next: AnsiStyle = { ...current }
	for (let index = 0; index < parts.length;) {
		const part = parts[index]
		if (part === '') {
			applySGRCode(next, 0)
			index++
			continue
		}
		if (!/^\d+$/.test(part)) {
			index++
			continue
		}
		const code = Number(part)
		if (!Number.isSafeInteger(code)) {
			index++
			continue
		}
		if (code === 38 || code === 48) {
			// Extended colors are intentionally unsupported, but their mode and
			// component parameters must be consumed as one group. Otherwise, for
			// example, the red component in 38;2;255;0;0 could become a reset or
			// the mode/component values could turn on text attributes.
			index += unsupportedColorGroupLength(parts, index)
			continue
		}
		applySGRCode(next, code)
		index++
	}
	return next
}

function unsupportedColorGroupLength(parts: string[], start: number): number {
	const mode = parts[start + 1]
	if (mode === '5') return hasNumericParameter(parts[start + 2]) ? 3 : Math.min(2, parts.length - start)
	if (mode === '2') {
		const components = parts.slice(start + 2, start + 5)
		if (components.length === 3 && components.every(hasNumericParameter)) return 5
		// Consume a malformed but recognizable color group too, so its partial
		// values cannot accidentally become independent SGR attributes.
		return Math.min(2 + components.length, parts.length - start)
	}
	return 1
}

function hasNumericParameter(value: string | undefined): boolean {
	return value !== undefined && /^\d+$/.test(value)
}

function applySGRCode(style: AnsiStyle, code: number): void {
	if (code === 0) {
		delete style.foreground
		delete style.background
		delete style.bold
		delete style.dim
		delete style.italic
		delete style.underline
		return
	}
	if (code === 1) { style.bold = true; return }
	if (code === 2) { style.dim = true; return }
	if (code === 3) { style.italic = true; return }
	if (code === 4) { style.underline = true; return }
	if (code === 22) { delete style.bold; delete style.dim; return }
	if (code === 23) { delete style.italic; return }
	if (code === 24) { delete style.underline; return }
	if (code === 39) { delete style.foreground; return }
	if (code === 49) { delete style.background; return }
	const foreground = foregroundColors[code]
	if (foreground) { style.foreground = foreground; return }
	const background = backgroundColors[code]
	if (background) style.background = background
}

function sameStyle(left: AnsiStyle, right: AnsiStyle): boolean {
	return left.foreground === right.foreground && left.background === right.background && left.bold === right.bold && left.dim === right.dim && left.italic === right.italic && left.underline === right.underline
}
