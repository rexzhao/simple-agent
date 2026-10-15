// The ExternalStoreRuntime adapter: the bridge between the serve wire
// protocol and assistant-ui. The queue is a MIRROR of server truth (the
// steered queue lives on the session, addressed by ItemID); enqueue /
// steer / edit / remove are network calls whose echoes arrive over SSE
// and update the lanes.
import { useMemo, useSyncExternalStore } from "react";
import type { AppendMessage, ExternalStoreAdapter } from "@assistant-ui/react";
import { APIError, postSend, postSteer, postStop, putSteer, deleteSteer } from "./api";
import { isSlashNew } from "./newsession";
import { convertLive, convertRecords, toThreadMessageLike, type ChatMessage } from "./convert";
import { SessionStore, type Snap } from "./store";
import type { ProjectsResponse } from "./types";

// Subscribing with no session selected: a stable no-op pair so
// useSyncExternalStore never sees changing identities across renders.
const noopSubscribe = () => () => {};
const emptySnapshot: Snap = {
  version: -1,
  session: "",
  records: [],
  live: null,
  steered: [],
  running: false,
  compacting: false,
  more: false,
  prefixAnchor: "",
  head: "",
  connected: false,
  verified: null,
  notice: "",
  tool: null,
};
const emptySnap = () => emptySnapshot;

function textOf(message: AppendMessage): string {
  if (typeof message.content === "string") return message.content;
  return message.content
    .filter(p => p.type === "text")
    .map(p => p.text)
    .join("");
}

export type SessionHandle = {
  store: SessionStore;
  snap: Snap;
};

// sendOrSteer implements the page's Rule A: the composer routes by the
// local running flag, but the flag can lag (a run starting or settling
// between render and POST), so a 409 crosses over to the other verb.
async function sendOrSteer(sessionID: string, running: boolean, text: string) {
  const primary = running ? () => postSteer(sessionID, text) : () => postSend(sessionID, text);
  const fallback = running ? () => postSend(sessionID, text) : () => postSteer(sessionID, text);
  try {
    await primary();
  } catch (e) {
    if (e instanceof APIError && e.status === 409) {
      await fallback();
      return;
    }
    throw e;
  }
}

export function useSaiAdapter(
  stores: Map<string, SessionStore>,
  sessionID: string,
  projects: ProjectsResponse | null,
  onSwitch: (id: string) => void,
  onDeleted: (id: string) => void,
  onSlashNew: () => void,
  onRequestDelete: (id: string, title: string) => void,
): ExternalStoreAdapter<ChatMessage> {
  let store = stores.get(sessionID) ?? (sessionID ? (stores.set(sessionID, new SessionStore()), stores.get(sessionID)!) : undefined);
  const handle = useSyncExternalStore(store ? store.subscribe : noopSubscribe, store ? store.getSnapshot : emptySnap);
  const snap = handle ?? emptySnapshot;

  return useMemo<ExternalStoreAdapter<ChatMessage>>(() => {
    const messages = convertRecords(snap.records);
    const live = convertLive(snap.live);
    if (live) messages.push(live);
    // Overlay the executing tool: its call part sits on the last
    // assistant message (the record landed before execution began).
    if (snap.tool) {
      for (let i = messages.length - 1; i >= 0; i--) {
        const m = messages[i];
        if (m.role !== "assistant") continue;
        const part = m.parts.find(p => p.type === "tool-call" && p.toolCallId === snap.tool?.callID);
        if (part && part.type === "tool-call") part.running = true;
        break;
      }
    }

    const sessionID = snap.session;
    const running = snap.running || snap.compacting;
    const activeStore = store;

    return {
      isRunning: running,
      isDisabled: !sessionID,
      messages,
      convertMessage: toThreadMessageLike,
      hasEarlier: snap.more,
      onLoadEarlier: async () => {
        // No scroll compensation needed — measured, not assumed: the
        // runtime anchors the prepend itself (smooth-scroll to the old
        // first message's shifted position, i.e. top + delta). The
        // "jump to mid-history" seen in testing was the test harness
        // scrolling the button into view before clicking; a real user
        // must scroll to the top to reach the button, which is exactly
        // the anchor base the runtime holds. An earlier stabilizer
        // patch here fought the runtime's animation — removed.
        await activeStore?.loadEarlier();
      },
      onNew: async (message: AppendMessage) => {
        const text = textOf(message);
        // EXACT /new opens the creation dialog instead of sending —
        // the CLI's exact-match contract carried into the chat
        // client ("/newfoo" is an ordinary message; so is
        // "/new -provider x": the dialog is the only creation
        // surface here).
        if (isSlashNew(text)) {
          onSlashNew();
          return;
        }
        await sendOrSteer(sessionID, running, text);
      },
      onCancel: async () => {
        await postStop(sessionID);
      },
      queue: {
        items: [],
        steerItems: snap.steered.map(s => ({
          id: s.item,
          prompt: s.text,
          parts: [{ type: "text" as const, text: s.text }],
        })),
        enqueue: (message: AppendMessage) => {
          void sendOrSteer(sessionID, running, textOf(message)).catch(console.error);
        },
        steer: (message: AppendMessage) => {
          void sendOrSteer(sessionID, true, textOf(message)).catch(console.error);
        },
        move: (queueItemId: string, placement: { lane?: "queue" | "steer"; insertAfter?: string | null; insertBefore?: string | null }) => {
          // The unanchored move into the steer lane is assistant-ui's
          // interrupt semantics ("run now"): stop the live run, withdraw
          // the item, and start a fresh run carrying its text — the
          // steered queue only drains at a run boundary, and a stopped
          // run leaves nothing to drain it, so "just stop" would park
          // the message forever. Anchored placement has no server
          // counterpart — the server queue is strictly FIFO by mint
          // time — so it stays a no-op.
          if (placement.lane === "steer" && placement.insertAfter == null && placement.insertBefore == null) {
            const item = snap.steered.find(s => s.item === queueItemId);
            if (!item || !sessionID) return;
            void (async () => {
              if (running) {
                try {
                  await postStop(sessionID);
                } catch (e) {
                  console.error(e);
                  return;
                }
              }
              // Withdraw first: the new run would drain the queue and
              // double the input. not_queued (a race the drain won)
              // is tolerable — the message is being processed anyway.
              await deleteSteer(sessionID, queueItemId).catch(() => {});
              for (let attempt = 0; attempt < 10; attempt++) {
                try {
                  await postSend(sessionID, item.text);
                  return;
                } catch (e) {
                  if (e instanceof APIError && e.status === 409) {
                    await new Promise(res => setTimeout(res, 300));
                    continue;
                  }
                  console.error(e);
                  return;
                }
              }
              console.error("run now: the run did not settle in time; the message is lost from the queue — resend it");
            })();
          }
        },
        edit: (queueItemId: string, message: AppendMessage) => {
          void putSteer(sessionID, queueItemId, textOf(message)).catch(console.error);
        },
        remove: (queueItemId: string) => {
          void deleteSteer(sessionID, queueItemId).catch(console.error);
        },
      },
      adapters: {
        threadList: {
          threadId: sessionID,
          threads: (projects?.projects || []).flatMap(p =>
            p.sessions.map(s => ({
              status: "regular" as const,
              id: s.id,
              title: s.title || "untitled",
            })),
          ),
          onSwitchToThread: (id: string) => {
            onSwitch(id);
          },
          onDelete: (id: string) => {
            // Deletion is confirmed first (C6a): the flow — detach
            // the open stream, bounded retries over the closing
            // window, refusal hints — lives in App, next to the
            // sidebar rows that trigger it.
            const entry = (projects?.projects ?? [])
              .flatMap(p => p.sessions)
              .find(sess => sess.id === id);
            onRequestDelete(id, entry?.title || "untitled");
          },
        },
      },
    };
  }, [snap, projects, onSwitch, onDeleted, onSlashNew, onRequestDelete]);
}
