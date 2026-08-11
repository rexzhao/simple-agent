import { describe, expect, it } from 'vitest'
import { ansiStyleClass, ansiToPlainText, parseAnsi } from './ansi'

describe('ANSI shell output parsing', () => {
	it('keeps plain text and meaningful whitespace unchanged', () => {
		const text = 'plain output\nsecond line\twith tabs'
		expect(ansiToPlainText(text)).toBe(text)
		expect(parseAnsi(text)).toEqual([{ text, style: {} }])
	})

	it('maps standard and bright colors, attributes, backgrounds, and reset', () => {
		const tokens = parseAnsi('\x1b[31mred\x1b[1;2;3;4mstyled\x1b[44mbackground\x1b[0mplain')
		expect(tokens).toEqual([
			{ text: 'red', style: { foreground: 'red' } },
			{ text: 'styled', style: { foreground: 'red', bold: true, dim: true, italic: true, underline: true } },
			{ text: 'background', style: { foreground: 'red', background: 'blue', bold: true, dim: true, italic: true, underline: true } },
			{ text: 'plain', style: {} },
		])

		const bright = parseAnsi('\x1b[91mbright red\x1b[103mbright yellow background\x1b[39;49mnormal')
		expect(bright[0].style).toEqual({ foreground: 'bright-red' })
		expect(bright[1].style).toEqual({ foreground: 'bright-red', background: 'bright-yellow' })
		expect(bright[2].style).toEqual({})
		expect(ansiStyleClass({ foreground: 'bright-cyan', background: 'blue', bold: true, dim: true, italic: true, underline: true })).toBe('ansi-output-token ansi-fg-bright-cyan ansi-bg-blue ansi-bold ansi-dim ansi-italic ansi-underline')
	})

	it('consumes unsupported extended color groups without leaking their parameters into styles', () => {
		const indexedForeground = parseAnsi('\x1b[38;5;1mindexed')
		expect(indexedForeground).toEqual([{ text: 'indexed', style: {} }])
		const trueColorForeground = parseAnsi('\x1b[38;2;255;0;0mtruecolor')
		expect(trueColorForeground).toEqual([{ text: 'truecolor', style: {} }])

		const indexedBackground = parseAnsi('\x1b[48;5;2mindexed background')
		expect(indexedBackground).toEqual([{ text: 'indexed background', style: {} }])
		const trueColorBackground = parseAnsi('\x1b[48;2;255;0;0mtruecolor background')
		expect(trueColorBackground).toEqual([{ text: 'truecolor background', style: {} }])

		const following = parseAnsi('\x1b[38;5;1;4munderline\x1b[0m\x1b[48;2;255;0;0;2mdim')
		expect(following[0].style).toEqual({ underline: true })
		expect(following[1].style).toEqual({ dim: true })
	})

	it('handles nested state changes without leaking styles past reset', () => {
		const tokens = parseAnsi('\x1b[32mgreen\x1b[1mbold\x1b[22mgreen again\x1b[39mnormal')
		expect(tokens).toEqual([
			{ text: 'green', style: { foreground: 'green' } },
			{ text: 'bold', style: { foreground: 'green', bold: true } },
			{ text: 'green again', style: { foreground: 'green' } },
			{ text: 'normal', style: {} },
		])
	})

	it('ignores non-style terminal controls and removes them for copying', () => {
		const text = 'before\x1b[2Jafter\x1b]0;unsafe title\x07tail\x1b[?25lend'
		expect(ansiToPlainText(text)).toBe('beforeaftertailend')
	})

	it('filters DEL and C1 control bytes from plain text', () => {
		const text = 'before\x7f\x80\x9bCSI payload\x9dOSC payload\x9cafter'
		const plain = ansiToPlainText(text)
		expect(plain).toBe('beforeCSI payloadOSC payloadafter')
		expect(plain).not.toMatch(/[\x7f-\x9f]/)
	})

	it('keeps HTML-looking shell text as text while stripping only ANSI', () => {
		const text = '\x1b[31m<img src=x onerror=alert(1)>\x1b[0m'
		expect(ansiToPlainText(text)).toBe('<img src=x onerror=alert(1)>')
		expect(parseAnsi(text)[0].text).toBe('<img src=x onerror=alert(1)>')
	})
})
