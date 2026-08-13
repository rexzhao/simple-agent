import { memo } from 'react'
import Markdown from 'react-markdown'
import type { Components } from 'react-markdown'
import remarkGfm from 'remark-gfm'

const markdownComponents: Components = {
  a: ({ node: _node, ...props }) => <a {...props} target="_blank" rel="noreferrer noopener" />,
}

/** The single visual authority for final, intermediate, and streaming
 * assistant prose. Surrounding rows own lifecycle chrome such as iteration
 * markers, cursors, and message actions. */
export const AssistantMarkdown = memo(function AssistantMarkdown({ text, streaming = false, cursor = false }: { text: string; streaming?: boolean; cursor?: boolean }) {
  return (
    <div className={`message-text markdown-body assistant-markdown ${streaming ? 'assistant-stream' : ''}`}>
      <Markdown remarkPlugins={[remarkGfm]} components={markdownComponents} skipHtml>{text}</Markdown>
      {cursor && <span className="cursor" aria-hidden="true" />}
    </div>
  )
})
