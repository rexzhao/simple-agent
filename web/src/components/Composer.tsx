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
  const composerDisabled = props.blocked || submitting
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
    if (props.blocked || submitting) return
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
        // Keep focus in the textarea so the user can keep typing (e.g. append
        // another message) without clicking back after Enter submits.
        textareaRef.current?.focus()
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
      setImageError(`You can attach up to ${maxPastedImageAttachments} images`)
      return
    }
    let totalBytes = props.draft.pastedImages.reduce((total, image) => total + image.sizeBytes, 0)
    for (const file of files) {
      if (!supportedPastedImageMediaTypes.has(file.type)) {
        setImageError(`Unsupported image format: ${file.type || 'unknown'}`)
        return
      }
      if (file.size === 0) {
        setImageError('Cannot attach an empty image')
        return
      }
      if (file.size > maxPastedImageBytes) {
        setImageError(`A single image cannot exceed ${formatBytes(maxPastedImageBytes)}`)
        return
      }
      totalBytes += file.size
      if (totalBytes > maxPastedImageTotalBytes) {
        setImageError(`Total image size cannot exceed ${formatBytes(maxPastedImageTotalBytes)}`)
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
    ? 'Append a message to the current run…'
    : props.blocked
      ? 'Another session is running. Switch back to check progress'
      : props.draft.pastedTexts.length > 0
        ? 'Add a note after the pasted text'
        : 'Send a message to SAI'

  return (
    <div className="composer-wrap">
      {props.draft.pastedTexts.length > 0 && (
        <div className="pasted-text-attachments" aria-label="Pasted text to send">
          {props.draft.pastedTexts.map((pastedText, index) => (
            <div className="pasted-text-attachment" key={pastedText.id}>
              <span className="pasted-text-attachment-icon"><PaperclipIcon /></span>
              <span className="pasted-text-attachment-copy">
                <strong>Pasted text #{index + 1}</strong>
                <small>{pastedTextSummary(pastedText.content)}</small>
              </span>
              <button
                type="button"
                className="pasted-text-attachment-remove"
                disabled={composerDisabled}
                onClick={() => props.onPastedTextRemove(pastedText.id)}
                aria-label={`Remove pasted text #${index + 1}`}
                title="Remove"
              >×</button>
            </div>
          ))}
        </div>
      )}
      {props.draft.pastedImages.length > 0 && (
        <div className="pasted-image-attachments" aria-label="Images to send">
          {props.draft.pastedImages.map((image, index) => (
            <div className="pasted-image-attachment" key={image.id}>
              <img src={image.dataURL} alt={`Image to send #${index + 1}`} />
              <span><strong>Image #{index + 1}</strong><small>{image.mediaType} · {formatBytes(image.sizeBytes)}</small></span>
              <button
                type="button"
                className="pasted-text-attachment-remove"
                disabled={composerDisabled}
                onClick={() => props.onPastedImageRemove(image.id)}
                aria-label={`Remove image #${index + 1}`}
                title="Remove"
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
            if (event.key === 'Enter' && !event.altKey) {
              event.preventDefault()
              void submit()
            }
          }}
        />
        {props.running && <button className="stop-button" onClick={props.onCancel} aria-label="Stop"><StopIcon /></button>}
		<button className="send-button" disabled={(!props.draft.content.trim() && props.draft.pastedTexts.length === 0 && props.draft.pastedImages.length === 0) || composerDisabled} onClick={() => void submit()} aria-label={props.running ? 'Append to current run' : 'Send'}><SendIcon /></button>
      </div>
      <div className="composer-hint"><span>{props.draft.pastedImages.length > 0 ? 'Images will be sent with the message' : props.draft.pastedTexts.length > 0 ? 'Pasted text is sent first, then your note' : 'Enter to send · Alt+Enter for a new line · Paste images supported'}</span><span>Running locally</span></div>
    </div>
  )
}

export { maxPastedImageAttachments }

function isLongPastedText(content: string): boolean {
  return content.split('\n').length > longPasteLineLimit || content.length > longPasteCharacterLimit
}

function pastedTextSummary(content: string): string {
  const lineCount = content.split('\n').length
  return lineCount > longPasteLineLimit ? `${lineCount.toLocaleString()} lines` : `${content.length.toLocaleString()} characters`
}
