import { useMemo } from 'react'
import { ansiStyleClass, parseAnsi } from '../lib/ansi'

export function AnsiOutput({ text, className }: { text: string; className?: string }) {
	const tokens = useMemo(() => parseAnsi(text), [text])
	const classes = ['ansi-output', className].filter(Boolean).join(' ')
	return (
		<pre className={classes}>
			{tokens.map((token, index) => <span className={ansiStyleClass(token.style)} key={index}>{token.text}</span>)}
		</pre>
	)
}
