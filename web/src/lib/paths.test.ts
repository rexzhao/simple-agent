import { describe, expect, it } from 'vitest'
import { isPathOutsideWorkspace } from './paths'

describe('isPathOutsideWorkspace', () => {
	it('treats relative targets as workspace content', () => {
		expect(isPathOutsideWorkspace('/repo', 'src/main.go')).toBe(false)
		expect(isPathOutsideWorkspace('/repo', '../sibling/secret.txt')).toBe(false)
		expect(isPathOutsideWorkspace('/repo', './a/./b')).toBe(false)
	})

	it('classifies POSIX absolute paths', () => {
		expect(isPathOutsideWorkspace('/repo', '/repo/src/main.go')).toBe(false)
		expect(isPathOutsideWorkspace('/repo', '/repo')).toBe(false)
		expect(isPathOutsideWorkspace('/repo', '/repo-other/file')).toBe(true)
		expect(isPathOutsideWorkspace('/repo', '/etc/passwd')).toBe(true)
	})

	it('matches the root as a path prefix, not a string prefix', () => {
		expect(isPathOutsideWorkspace('/repo', '/repossession/x')).toBe(true)
		expect(isPathOutsideWorkspace('/repo/', '/repo/x')).toBe(false)
	})

	it('normalizes separators and dot segments before comparing', () => {
		expect(isPathOutsideWorkspace('/repo', '/repo/./src//main.go')).toBe(false)
		expect(isPathOutsideWorkspace('/repo', '/repo/sub/../main.go')).toBe(false)
		expect(isPathOutsideWorkspace('/repo', '/repo/sub/../../elsewhere/x')).toBe(true)
	})

	it('compares Windows paths case-insensitively across slash styles', () => {
		expect(isPathOutsideWorkspace('F:\\work\\lua-x', 'f:/work/lua-x/src/main.rs')).toBe(false)
		expect(isPathOutsideWorkspace('F:/work/lua-x', 'F:\\work\\lua-x')).toBe(false)
		expect(isPathOutsideWorkspace('F:/work/lua-x', 'C:/Users/rex/file.txt')).toBe(true)
		expect(isPathOutsideWorkspace('F:/work/lua-x', 'F:/work/lua-x-other/file')).toBe(true)
	})

	it('handles blank input', () => {
		expect(isPathOutsideWorkspace('', '/etc/passwd')).toBe(false)
		expect(isPathOutsideWorkspace('/repo', '')).toBe(false)
	})
})
