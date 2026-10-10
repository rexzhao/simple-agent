import { useEffect, useMemo, useState } from "react";
import { useSyncExternalStore } from "react";
import {
  AssistantRuntimeProvider,
  useExternalStoreRuntime,
} from "@assistant-ui/react";
import { TooltipProvider } from "@/components/ui/tooltip";
import { Thread } from "@/components/thread.aui";
import { getProjects } from "./api";
import { useSaiAdapter } from "./runtime";
import { SessionStore } from "./store";
import type { ProjectsResponse } from "./types";

// ---- projects sidebar (own poll, like the log page) ----

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

// ---- app ----

export default function App() {
  const projects = useProjects();
  const [sessionID, setSessionID] = useState("");
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

  const adapter = useSaiAdapter(
    stores,
    sessionID,
    projects,
    setSessionID,
    (id) => {
      const store = stores.get(id);
      if (store) {
        store.destroy();
        stores.delete(id);
      }
    },
  );
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
                <button
                  key={s.id}
                  className={
                    "flex cursor-pointer flex-col gap-0.5 rounded-lg border-none px-3 py-2 text-left font-sans text-sm " +
                    (s.id === sessionID
                      ? "bg-accent text-accent-foreground"
                      : "bg-transparent hover:bg-accent/50")
                  }
                  onClick={() => setSessionID(s.id)}
                  title={s.path}
                >
                  <span className="truncate">{s.title}</span>
                  <span className="text-muted-foreground truncate text-xs">
                    {s.path.replace(/[\\/]$/, "").split(/[\\/]/).pop()}
                  </span>
                </button>
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
      </AssistantRuntimeProvider>
    </TooltipProvider>
  );
}
