import { useCallback, useEffect, useMemo, useState } from "react";
import { useSyncExternalStore } from "react";
import {
  AssistantRuntimeProvider,
  useExternalStoreRuntime,
} from "@assistant-ui/react";
import { TooltipProvider } from "@/components/ui/tooltip";
import { Thread } from "@/components/thread.aui";
import { APIError, deleteSession, getModels, getProjects } from "./api";
import { useSaiAdapter } from "./runtime";
import { SessionStore } from "./store";
import { convertRecords } from "./convert";
import {
  deleteBlockedHint,
  deleteRetryable,
  type ModelDescriptor,
} from "./newsession";
import { NewSessionDialog } from "./components/new-session-dialog";
import { ConfirmDeleteDialog } from "./components/confirm-delete-dialog";
import type { ProjectsResponse } from "./types";

// ---- projects sidebar (own poll, like the log page) ----

// useModels fetches the selectable models once (§17.2): the list is
// configuration, not live state — a change is a config reload away
// and the dialog can live with a stale copy until the next open.
function useModels(): ModelDescriptor[] | null {
  const [models, setModels] = useState<ModelDescriptor[] | null>(null);
  useEffect(() => {
    let stop = false;
    getModels()
      .then((m) => !stop && setModels(m))
      .catch(() => {
        /* the dialog shows "no models" — the poll will not fix a dead serve */
      });
    return () => {
      stop = true;
    };
  }, []);
  return models;
}

function useProjects(): ProjectsResponse | null {
  const [projects, setProjects] = useState<ProjectsResponse | null>(null);
  useEffect(() => {
    let stop = false;
    const tick = async () => {
      try {
        const p = await getProjects();
        if (!stop) setProjects(p);
      } catch {
        /* keep last good */
      }
    };
    void tick();
    const timer = setInterval(tick, 5000);
    return () => {
      stop = true;
      clearInterval(timer);
    };
  }, []);
  return projects;
}

// The session route, shared with the log page's convention.
function parseSessionHash(): string {
  const m = location.hash.match(/^#\/s\/([a-z0-9]+)$/);
  return m ? m[1] : "";
}

// ---- app ----

export default function App() {
  const projects = useProjects();
  const models = useModels();
  // Hash routing, same contract as the log page: #/s/<id> selects a
  // session (deep links, back/forward, and automation written against
  // the log page's convention all work); anything else is the list.
  const [sessionID, setSessionIDState] = useState(() => parseSessionHash());
  const setSessionID = useCallback((id: string) => {
    setSessionIDState(id);
    // Writing the hash fires hashchange, which re-selects the same id
    // (a no-op) — idempotent by construction.
    if (id) location.hash = "/s/" + id;
    else if (location.hash) history.replaceState(null, "", location.pathname);
  }, []);
  useEffect(() => {
    const onHash = () => setSessionIDState(parseSessionHash());
    window.addEventListener("hashchange", onHash);
    return () => window.removeEventListener("hashchange", onHash);
  }, []);
  const stores = useMemo(() => new Map<string, SessionStore>(), []);

  // Detach every store we are not looking at (each live SSE stream
  // holds server-side refs); attach the current one.
  useEffect(() => {
    for (const [id, store] of stores) {
      if (id !== sessionID) store.detach();
    }
    const store = stores.get(sessionID);
    // An empty session id (nothing selected) must never open a stream:
    // /api/s//stream normalizes to /api/s/stream — a 404 that looks
    // like a routing bug.
    if (store && sessionID) void store.open(sessionID);
  }, [sessionID, stores]);

  useEffect(
    () => () => {
      for (const store of stores.values()) store.destroy();
    },
    [stores],
  );

  const onDeleted = useCallback(
    (id: string) => {
      const store = stores.get(id);
      if (store) {
        store.destroy();
        stores.delete(id);
      }
    },
    [stores],
  );

  // ---- new-session dialog (C6a) ----
  const [newOpen, setNewOpen] = useState(false);
  const openNew = useCallback(() => setNewOpen(true), []);
  const currentProject = useMemo(() => {
    for (const p of projects?.projects || []) {
      if (p.sessions.some((sess) => sess.id === sessionID)) return p.id;
    }
    return projects?.projects[0]?.id ?? "";
  }, [projects, sessionID]);

  // ---- delete confirmation (C6a) ----
  const [deleteTarget, setDeleteTarget] = useState<{ id: string; title: string } | null>(null);
  const [deleteHint, setDeleteHint] = useState("");
  const [deleteBusy, setDeleteBusy] = useState(false);
  const requestDelete = useCallback((id: string, title: string) => {
    setDeleteHint("");
    setDeleteTarget({ id, title });
  }, []);
  const confirmDelete = useCallback(async () => {
    if (!deleteTarget) return;
    setDeleteBusy(true);
    const id = deleteTarget.id;
    // Deleting the open session first navigates away (the stream
    // holds server-side refs → session_open); bounded retries cover
    // the closing window, the same flow the runtime used to own.
    if (id === sessionID) setSessionID("");
    for (let attempt = 0; attempt < 10; attempt++) {
      try {
        await deleteSession(id);
        onDeleted(id);
        setDeleteTarget(null);
        return;
      } catch (e) {
        if (e instanceof APIError && deleteRetryable(e.code)) {
          await new Promise((res) => setTimeout(res, 500));
          continue;
        }
        const hint = e instanceof APIError ? deleteBlockedHint(e) : null;
        setDeleteHint(hint ?? "delete failed — try again");
        return;
      } finally {
        setDeleteBusy(false);
      }
    }
    setDeleteHint("the session stayed open — try again in a moment");
  }, [deleteTarget, sessionID, setSessionID, onDeleted]);

  const adapter = useSaiAdapter(stores, sessionID, projects, setSessionID, onDeleted, openNew, requestDelete);
  const runtime = useExternalStoreRuntime(adapter);

  // The status strip reads the current session's snapshot directly —
  // the same store bumps that re-render the thread re-render this.
  const store = stores.get(sessionID);
  const snap = useSyncExternalStore(
    store ? store.subscribe : () => () => {},
    store ? store.getSnapshot : () => null,
  );

  const sessions = useMemo(() => {
    const out: { id: string; title: string; project: string; path: string }[] = [];
    for (const p of projects?.projects || []) {
      for (const s of p.sessions)
        out.push({
          id: s.id,
          title: s.title || "untitled",
          project: p.id,
          // The registered path (tail segment as the visible label,
          // full path on the tooltip) — a project hash reads as noise.
          path: p.path,
        });
    }
    return out;
  }, [projects]);

  const verified = snap ? snap.verified : null;
  const notice = snap ? snap.notice : "";

  const saiConsole = {
    list: () => [...stores.entries()].map(([id, s]) => ({
      id,
      connected: s.snap.connected,
      records: s.snap.records.length,
      running: s.snap.running,
      verified: s.snap.verified,
      head: s.snap.head.slice(0, 12),
    })),
    snap: (id?: string) => stores.get(id ?? sessionID)?.getSnapshot() ?? null,
    messages: (id?: string) => {
      const s = stores.get(id ?? sessionID);
      return s ? convertRecords(s.getSnapshot().records) : [];
    },
    select: (id: string) => setSessionID(id),
  };
  (window as unknown as Record<string, unknown>).sai = saiConsole;

  return (
    <TooltipProvider>
      <AssistantRuntimeProvider runtime={runtime}>
        <div className="bg-background text-foreground flex h-screen w-full overflow-hidden">
          <nav className="bg-background border-border hidden w-64 shrink-0 flex-col border-r md:flex">
            <div className="text-muted-foreground border-border flex items-baseline justify-between border-b px-4 py-3 text-xs tracking-wider uppercase">
              sessions
              <a
                className="hover:text-foreground no-underline"
                href="/log"
                title="the record-level log view"
              >
                /log
              </a>
            </div>
            <div className="flex flex-col gap-0.5 overflow-y-auto p-2">
              {sessions.length === 0 && (
                <div className="text-muted-foreground px-3 py-4 text-sm">
                  no sessions — create one from the CLI
                </div>
              )}
              {sessions.map(s => (
                <div
                  key={s.id}
                  className={
                    "group flex items-stretch gap-0.5 rounded-lg font-sans text-sm " +
                    (s.id === sessionID ? "bg-accent text-accent-foreground" : "hover:bg-accent/50")
                  }
                  title={s.path}
                >
                  <button
                    className={
                      "flex min-w-0 flex-1 cursor-pointer flex-col gap-0.5 border-none bg-transparent px-3 py-2 text-left " +
                      (s.id === sessionID
                        ? "text-accent-foreground"
                        : "bg-transparent")
                    }
                    onClick={() => setSessionID(s.id)}
                  >
                    <span className="truncate">{s.title}</span>
                    <span className="text-muted-foreground truncate text-xs">
                      {s.path.replace(/[\\/]$/, "").split(/[\\/]/).pop()}
                    </span>
                  </button>
                  <button
                    className="text-muted-foreground hover:text-destructive invisible mr-1 cursor-pointer self-start border-none bg-transparent px-1 pt-2 text-xs group-hover:visible"
                    onClick={e => {
                      e.stopPropagation();
                      requestDelete(s.id, s.title);
                    }}
                    aria-label={`delete session ${s.title || "untitled"}`}
                    title="delete this session"
                  >
                    ×
                  </button>
                </div>
              ))}
            </div>
          </nav>
          <div className="relative flex min-w-0 flex-1 flex-col">
            <div className="text-muted-foreground border-border flex min-h-7 items-baseline gap-4 border-b px-4 py-1 text-xs">
              <span
                className={
                  verified === false ? "text-destructive font-semibold" : ""
                }
              >
                {verified === false ? "chain drift!" : verified ? "verified" : "…"}
              </span>
              {notice && <span className="text-accent-foreground">{notice}</span>}
            </div>
            <div className="aui-root relative flex min-h-0 flex-1 flex-col items-center justify-start">
              <Thread autoFocus />
            </div>
          </div>
        </div>
        <NewSessionDialog
          open={newOpen}
          onClose={() => setNewOpen(false)}
          projects={(projects?.projects ?? []).map((p) => ({ id: p.id, path: p.path }))}
          models={models ?? []}
          currentProject={currentProject}
          onCreated={setSessionID}
        />
        <ConfirmDeleteDialog
          target={deleteTarget}
          onCancel={() => setDeleteTarget(null)}
          onConfirm={() => void confirmDelete()}
          hint={deleteHint}
          busy={deleteBusy}
        />
      </AssistantRuntimeProvider>
    </TooltipProvider>
  );
}
