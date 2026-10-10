// The ExternalStoreRuntime adapter: the bridge between the serve wire
// protocol and assistant-ui. The queue is a MIRROR of server truth (the
// steered queue lives on the session, addressed by ItemID); enqueue /
// steer / edit / remove are network calls whose echoes arrive over SSE
// and update the lanes.
import { useMemo, useSyncExternalStore } from "react";
import type { AppendMessage, ExternalStoreAdapter } from "@assistant-ui/react";
import { APIError, deleteSession, postSend, postSteer, postStop, putSteer, deleteSteer } from "./api";
import { convertLive, convertRecords, toThreadMessageLike, type ChatMessage } from "./convert";
import { SessionStore, type Snap } from "./store";
import type { ProjectsResponse } from "./types";

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
): ExternalStoreAdapter<ChatMessage> {
  let store = stores.get(sessionID);
  if (!store) {
    store = new SessionStore();
    stores.set(sessionID, store);
  }
  const handle = useSyncExternalStore(store.subscribe, store.getSnapshot);
  const snap = handle;

  return useMemo<ExternalStoreAdapter<ChatMessage>>(() => {
    const messages = convertRecords(snap.records);
    const live = convertLive(snap.live);
    if (live) messages.push(live);

    const sessionID = snap.session;
    const running = snap.running || snap.compacting;

    return {
      isRunning: running,
      isDisabled: !sessionID,
      messages,
      convertMessage: toThreadMessageLike,
      hasEarlier: snap.more,
      onLoadEarlier: async () => {
        await store.loadEarlier();
      },
      onNew: async (message: AppendMessage) => {
        await sendOrSteer(sessionID, running, textOf(message));
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
        move: (_queueItemId: string, placement: { lane?: "queue" | "steer"; insertAfter?: string | null; insertBefore?: string | null }) => {
          // The unanchored move into the steer lane is assistant-ui's
          // interrupt semantics ("run now"): stop the live run; the
          // item stays queued server-side and drains at the next run
          // boundary. Anchored placement has no server counterpart —
          // the server queue is strictly FIFO by mint time — so it is
          // a no-op here.
          if (placement.lane === "steer" && placement.insertAfter == null && placement.insertBefore == null && running) {
            void postStop(sessionID).catch(console.error);
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
              title: s.title || p.id,
            })),
          ),
          onSwitchToThread: (id: string) => {
            onSwitch(id);
          },
          onDelete: (id: string) => {
            // Deleting the open session first detaches (the SSE stream
            // holds server-side refs → session_open); bounded retries
            // cover the closing window.
            void (async () => {
              if (id === sessionID) onSwitch("");
              for (let attempt = 0; attempt < 10; attempt++) {
                try {
                  await deleteSession(id);
                  onDeleted(id);
                  return;
                } catch (e) {
                  if (e instanceof APIError && (e.code === "session_open" || e.code === "session_closing")) {
                    await new Promise(res => setTimeout(res, 500));
                    continue;
                  }
                  console.error(e);
                  return;
                }
              }
            })();
          },
        },
      },
    };
  }, [snap, projects, onSwitch, onDeleted]);
}
