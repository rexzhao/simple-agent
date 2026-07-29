import { describe, expect, it } from 'vitest'
import { isSessionToolName, prettyJSONText, sessionToolPhrase, sessionToolTarget } from './sessionTools'

const sessionNames = {
	'20260729T031859.164118400Z-ff61fe47': 'M10 compilation boundary fix review',
}

describe('isSessionToolName', () => {
	it('recognizes the session orchestration tools', () => {
		expect(isSessionToolName('session_start')).toBe(true)
		expect(isSessionToolName('session_history')).toBe(true)
		expect(isSessionToolName('read_file')).toBe(false)
		expect(isSessionToolName('session')).toBe(false)
	})
})

describe('sessionToolTarget', () => {
	it('summarizes session_start with name and model', () => {
		expect(sessionToolTarget('session_start', { name: 'Review', model: 'grok-4.5', prompt: 'please review' }, sessionNames))
			.toBe('"Review" · grok-4.5')
	})

	it('falls back to a prompt snippet for unnamed session_start', () => {
		expect(sessionToolTarget('session_start', { prompt: 'line one\nline two' })).toBe('line one line two')
	})

	it('truncates long prompts in session_start', () => {
		const target = sessionToolTarget('session_start', { prompt: 'x'.repeat(200) })
		expect(target).toHaveLength(81)
		expect(target.endsWith('…')).toBe(true)
	})

	it('summarizes session_search with the regex and archived flag', () => {
		expect(sessionToolTarget('session_search', { name_regex: 'Dev' })).toBe('/Dev/')
		expect(sessionToolTarget('session_search', { name_regex: '.*', include_archived: true })).toBe('/.*\/ · archived')
	})

	it('resolves target session display names', () => {
		expect(sessionToolTarget('session_get', { session_id: '20260729T031859.164118400Z-ff61fe47' }, sessionNames))
			.toBe('M10 compilation boundary fix review')
	})

	it('falls back to the raw session id when no name is known', () => {
		expect(sessionToolTarget('session_wait', { session_id: 'unknown-id', timeout_ms: 1000 }, sessionNames)).toBe('unknown-id')
		expect(sessionToolTarget('session_stop', { session_id: 'unknown-id' })).toBe('unknown-id')
	})

	it('shows history cursors so conflicting calls are visible collapsed', () => {
		const id = '20260729T031859.164118400Z-ff61fe47'
		expect(sessionToolTarget('session_history', { session_id: id, cursor: 120, direction: 'before' }, sessionNames))
			.toBe('M10 compilation boundary fix review · before 120')
		expect(sessionToolTarget('session_history', { session_id: id, cursor: 208, direction: 'after' }, sessionNames))
			.toBe('M10 compilation boundary fix review · after 208')
		expect(sessionToolTarget('session_history', { session_id: id, cursor: 208 }, sessionNames))
			.toBe('M10 compilation boundary fix review · cursor 208')
		// Retired before_seq/after_seq arguments still render for historical calls.
		expect(sessionToolTarget('session_history', { session_id: id, before_seq: 1, after_seq: 208 }, sessionNames))
			.toBe('M10 compilation boundary fix review · before 1 · after 208')
		expect(sessionToolTarget('session_history', { session_id: id }, sessionNames))
			.toBe('M10 compilation boundary fix review')
	})

	it('summarizes session_send with mode and a message snippet', () => {
		expect(sessionToolTarget('session_send', { session_id: 'id-1', mode: 'queue', message: 'hello\nworld' }, {}))
			.toBe('id-1 · queue · "hello world"')
	})

	it('returns an empty summary for session_models', () => {
		expect(sessionToolTarget('session_models', {})).toBe('')
	})
})

describe('sessionToolPhrase', () => {
	it('phrases singular and plural counts', () => {
		expect(sessionToolPhrase('session_start', 1)).toBe('Started 1 session')
		expect(sessionToolPhrase('session_start', 3)).toBe('Started 3 sessions')
		expect(sessionToolPhrase('session_search', 1)).toBe('Searched sessions')
		expect(sessionToolPhrase('session_get', 2)).toBe('Inspected sessions ×2')
		expect(sessionToolPhrase('session_history', 1)).toBe('Read session history')
		expect(sessionToolPhrase('session_send', 2)).toBe('Sent 2 session messages')
		expect(sessionToolPhrase('session_wait', 1)).toBe('Waited on a session')
		expect(sessionToolPhrase('session_stop', 2)).toBe('Stopped 2 sessions')
		expect(sessionToolPhrase('session_models', 2)).toBe('Listed session models')
	})

	it('falls back to the raw name for unknown tools', () => {
		expect(sessionToolPhrase('mystery', 1)).toBe('mystery')
		expect(sessionToolPhrase('mystery', 4)).toBe('mystery ×4')
	})
})

describe('prettyJSONText', () => {
	it('pretty-prints JSON objects', () => {
		expect(prettyJSONText('{"ok":true,"n":1}')).toBe('{\n  "ok": true,\n  "n": 1\n}')
	})

	it('leaves non-JSON text untouched', () => {
		expect(prettyJSONText('plain output')).toBe('plain output')
		expect(prettyJSONText('')).toBe('')
	})
})
