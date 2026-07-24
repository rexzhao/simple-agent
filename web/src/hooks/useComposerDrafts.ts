import { useCallback, useState } from 'react'
import { emptyComposerDraft, maxPastedImageAttachments } from '../components/Composer'
import type { ComposerDraft, PastedImageAttachment, PastedTextAttachment } from '../components/Composer'

export function useComposerDrafts() {
  const [draftsBySession, setDraftsBySession] = useState<Record<string, ComposerDraft>>({})

  const updateDraft = useCallback((sessionID: string, content: string) => setDraftsBySession((current) => {
    const draft = current[sessionID] ?? emptyComposerDraft
    return draft.content === content ? current : { ...current, [sessionID]: { ...draft, content } }
  }), [])
  const addPastedText = useCallback((sessionID: string, item: PastedTextAttachment) => setDraftsBySession((current) => {
    const draft = current[sessionID] ?? emptyComposerDraft
    return { ...current, [sessionID]: { ...draft, pastedTexts: [...draft.pastedTexts, item] } }
  }), [])
  const removePastedText = useCallback((sessionID: string, id: number) => setDraftsBySession((current) => {
    const draft = current[sessionID]
    if (!draft?.pastedTexts.some((item) => item.id === id)) return current
    return { ...current, [sessionID]: { ...draft, pastedTexts: draft.pastedTexts.filter((item) => item.id !== id) } }
  }), [])
  const addPastedImage = useCallback((sessionID: string, item: PastedImageAttachment) => setDraftsBySession((current) => {
    const draft = current[sessionID] ?? emptyComposerDraft
    if (draft.pastedImages.length >= maxPastedImageAttachments) return current
    return { ...current, [sessionID]: { ...draft, pastedImages: [...draft.pastedImages, item] } }
  }), [])
  const removePastedImage = useCallback((sessionID: string, id: number) => setDraftsBySession((current) => {
    const draft = current[sessionID]
    if (!draft?.pastedImages.some((item) => item.id === id)) return current
    return { ...current, [sessionID]: { ...draft, pastedImages: draft.pastedImages.filter((item) => item.id !== id) } }
  }), [])
  const clearDraft = useCallback((sessionID: string) => setDraftsBySession((current) => (
    current[sessionID] ? { ...current, [sessionID]: emptyComposerDraft } : current
  )), [])

  return { draftsBySession, updateDraft, addPastedText, removePastedText, addPastedImage, removePastedImage, clearDraft }
}
