// Workspace path classification for the conversation UI. Full access
// sessions may address files outside the workspace; the timeline marks those
// targets so out-of-workspace reads and writes stay visible at a glance.

// isPathOutsideWorkspace reports whether target points outside root. Relative
// targets (including ../ escapes) are treated as workspace content: the file
// tools anchor them at the workspace, and flagging them would cry wolf. Both
// POSIX (/…) and Windows (C:\… / C:/…, case-insensitive) forms are handled.
export function isPathOutsideWorkspace(root: string, target: string): boolean {
	const normalizedRoot = normalizePath(root)
	const normalizedTarget = normalizePath(target)
	if (!normalizedRoot || !normalizedTarget) return false
	if (!isAbsolutePath(normalizedTarget)) return false
	const windows = isWindowsPath(normalizedRoot)
	const rootKey = windows ? normalizedRoot.toLowerCase() : normalizedRoot
	const targetKey = windows ? normalizedTarget.toLowerCase() : normalizedTarget
	if (targetKey === rootKey) return false
	return !targetKey.startsWith(rootKey + '/')
}

function isAbsolutePath(path: string): boolean {
	return path.startsWith('/') || isWindowsPath(path)
}

function isWindowsPath(path: string): boolean {
	return /^[A-Za-z]:\//.test(path)
}

// normalizePath converts backslashes to slashes and resolves . and ..
// segments lexically, so equivalent spellings of the same path compare equal.
function normalizePath(path: string): string {
	const slashed = path.trim().replace(/\\/g, '/')
	if (!slashed) return ''
	const isAbs = slashed.startsWith('/')
	const drive = /^[A-Za-z]:/.test(slashed) ? slashed.slice(0, 2) : ''
	const body = drive ? slashed.slice(2) : slashed
	const segments: string[] = []
	for (const segment of body.split('/')) {
		if (!segment || segment === '.') continue
		if (segment === '..') {
			if (segments.length > 0 && segments[segments.length - 1] !== '..') {
				segments.pop()
			} else if (!isAbs && !drive) {
				segments.push('..')
			}
			continue
		}
		segments.push(segment)
	}
	const prefix = drive || (isAbs ? '/' : '')
	const joined = segments.join('/')
	if (!prefix) return joined
	return joined ? `${prefix}${prefix === '/' ? '' : '/'}${joined}` : prefix
}
