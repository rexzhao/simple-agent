import { useCallback, useEffect, useRef, useState } from 'react'
import { blobAsDataURL, errorMessage, formatBytes } from '../lib/format'
import { PaperclipIcon, SendIcon, StopIcon } from './icons'

export interface PastedTextAttachment {
  id: number
  content: string
}

export interface PastedImageAttachment {
  id: number
  dataURL: string
  mediaType: string
  sizeBytes: number
}

export interface ComposerDraft {
  content: string
  pastedTexts: PastedTextAttachment[]
  pastedImages: PastedImageAttachment[]
}

export const emptyComposerDraft: ComposerDraft = { content: '', pastedTexts: [], pastedImages: [] }

const longPasteLineLimit = 10
const longPasteCharacterLimit = 1000
const maxPastedImageAttachments = 5
const maxPastedImageBytes = 4 * 1024 * 1024
const maxPastedImageTotalBytes = 12 * 1024 * 1024
const supportedPastedImageMediaTypes = new Set(['image/png', 'image/jpeg', 'image/gif', 'image/webp'])

export function Composer(props: {
  draft: ComposerDraft
  onContentChange: (content: string) => void
  onPastedTextAdd: (pastedText: PastedTextAttachment) => void
  onPastedTextRemove: (pastedTextID: number) => void
  onPastedImageAdd: (pastedImage: PastedImageAttachment) => void
  onPastedImageRemove: (pastedImageID: number) => void
  onDraftClear: () => void
  running: boolean
  blocked: boolean
  onSend: (content: string, images: PastedImageAttachment[]) => Promise<boolean>
  onCancel: () => void
}) {
  const textareaRef = useRef<HTMLTextAreaElement>(null)
  const nextPastedTextID = useRef(1)
  const nextPastedImageID = useRef(1)
  const [imageError, setImageError] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const composerDisabled = props.running || props.blocked || submitting
  const resizeTextarea = useCallback(() => {
    const textarea = textareaRef.current
    if (!textarea) return
    textarea.style.height = 'auto'
    textarea.style.height = `${textarea.scrollHeight}px`
  }, [])

  useEffect(() => {
    resizeTextarea()
  }, [props.draft.content, resizeTextarea])

  useEffect(() => {
    window.addEventListener('resize', resizeTextarea)
    return () => window.removeEventListener('resize', resizeTextarea)
  }, [resizeTextarea])

  const submit = async () => {
    if (props.running || props.blocked || submitting) return
    const content = [...props.draft.pastedTexts.map((pastedText) => pastedText.content), props.draft.content]
      .filter((part) => part.trim())
      .join('\n\n')
      .trim()
    if (!content && props.draft.pastedImages.length === 0) return
    setSubmitting(true)
    try {
      if (await props.onSend(content, props.draft.pastedImages)) {
        props.onDraftClear()
        setImageError('')
      }
    } catch (reason) {
      setImageError(errorMessage(reason))
    } finally {
      setSubmitting(false)
    }
  }

  const addClipboardImages = async (files: File[]) => {
    setImageError('')
    if (props.draft.pastedImages.length+files.length > maxPastedImageAttachments) {
      setImageError(`最多可附加 ${maxPastedImageAttachments} 张图片`)
      return
    }
    let totalBytes = props.draft.pastedImages.reduce((total, image) => total + image.sizeBytes, 0)
    for (const file of files) {
      if (!supportedPastedImageMediaTypes.has(file.type)) {
        setImageError(`不支持 ${file.type || '未知'} 图片格式`)
        return
      }
      if (file.size === 0) {
        setImageError('不能附加空图片')
        return
      }
      if (file.size > maxPastedImageBytes) {
        setImageError(`单张图片不能超过 ${formatBytes(maxPastedImageBytes)}`)
        return
      }
      totalBytes += file.size
      if (totalBytes > maxPastedImageTotalBytes) {
        setImageError(`图片总大小不能超过 ${formatBytes(maxPastedImageTotalBytes)}`)
        return
      }
    }
    try {
      for (const file of files) {
        props.onPastedImageAdd({
          id: nextPastedImageID.current++,
          dataURL: await blobAsDataURL(file),
          mediaType: file.type,
          sizeBytes: file.size,
        })
      }
    } catch (reason) {
      setImageError(errorMessage(reason))
    }
  }

  const placeholder = props.running
    ? 'SAI 正在执行…'
    : props.blocked
      ? '另一个会话正在执行，可切回查看进度'
      : props.draft.pastedTexts.length > 0
        ? '在粘贴文本后补充说明'
        : '给 SAI 发送消息'

  return (
    <div className="composer-wrap">
      {props.draft.pastedTexts.length > 0 && (
        <div className="pasted-text-attachments" aria-label="待发送的粘贴文本">
          {props.draft.pastedTexts.map((pastedText, index) => (
            <div className="pasted-text-attachment" key={pastedText.id}>
              <span className="pasted-text-attachment-icon"><PaperclipIcon /></span>
              <span className="pasted-text-attachment-copy">
                <strong>粘贴文本 #{index + 1}</strong>
                <small>{pastedTextSummary(pastedText.content)}</small>
              </span>
              <button
                type="button"
                className="pasted-text-attachment-remove"
                disabled={composerDisabled}
                onClick={() => props.onPastedTextRemove(pastedText.id)}
                aria-label={`移除粘贴文本 #${index + 1}`}
                title="移除"
              >×</button>
            </div>
          ))}
        </div>
      )}
      {props.draft.pastedImages.length > 0 && (
        <div className="pasted-image-attachments" aria-label="待发送的图片">
          {props.draft.pastedImages.map((image, index) => (
            <div className="pasted-image-attachment" key={image.id}>
              <img src={image.dataURL} alt={`待发送图片 #${index + 1}`} />
              <span><strong>图片 #{index + 1}</strong><small>{image.mediaType} · {formatBytes(image.sizeBytes)}</small></span>
              <button
                type="button"
                className="pasted-text-attachment-remove"
                disabled={composerDisabled}
                onClick={() => props.onPastedImageRemove(image.id)}
                aria-label={`移除图片 #${index + 1}`}
                title="移除"
              >×</button>
            </div>
          ))}
        </div>
      )}
      {imageError && <div className="pasted-image-error" role="alert">{imageError}</div>}
      <div className="composer">
        <textarea
          ref={textareaRef}
          value={props.draft.content}
		  disabled={composerDisabled}
          rows={1}
		  placeholder={placeholder}
          onChange={(event) => props.onContentChange(event.target.value)}
          onPaste={(event) => {
            const imageFiles = Array.from(event.clipboardData.items)
              .filter((item) => item.kind === 'file' && item.type.startsWith('image/'))
              .map((item) => item.getAsFile())
              .filter((file): file is File => file !== null)
            if (imageFiles.length > 0) {
              event.preventDefault()
              void addClipboardImages(imageFiles)
              return
            }
            const pastedText = event.clipboardData.getData('text/plain').replace(/\r\n?/g, '\n')
            if (!isLongPastedText(pastedText)) return
            event.preventDefault()
            props.onPastedTextAdd({ id: nextPastedTextID.current++, content: pastedText })
          }}
          onKeyDown={(event) => {
            if (event.key === 'Enter' && !event.shiftKey) {
              event.preventDefault()
              void submit()
            }
          }}
        />
        {props.running ? (
          <button className="stop-button" onClick={props.onCancel}><StopIcon /> 停止</button>
        ) : (
		  <button className="send-button" disabled={(!props.draft.content.trim() && props.draft.pastedTexts.length === 0 && props.draft.pastedImages.length === 0) || composerDisabled} onClick={() => void submit()} aria-label="发送"><SendIcon /></button>
        )}
      </div>
      <div className="composer-hint"><span>{props.draft.pastedImages.length > 0 ? '图片将与消息一起发送' : props.draft.pastedTexts.length > 0 ? '粘贴文本会先发送，补充说明随后附加' : 'Enter 发送 · Shift+Enter 换行 · 可粘贴图片'}</span><span>本地运行</span></div>
    </div>
  )
}

export { maxPastedImageAttachments }

function isLongPastedText(content: string): boolean {
  return content.split('\n').length > longPasteLineLimit || content.length > longPasteCharacterLimit
}

function pastedTextSummary(content: string): string {
  const lineCount = content.split('\n').length
  return lineCount > longPasteLineLimit ? `${lineCount.toLocaleString()} 行` : `${content.length.toLocaleString()} 字符`
}
