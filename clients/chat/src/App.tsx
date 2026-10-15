import { useCallback, useEffect, useMemo, useState } from "react";
import { useSyncExternalStore } from "react";
import {
  AssistantRuntimeProvider,
  useExternalStoreRuntime,
} from "@assistant-ui/react";
import { TooltipProvider } from "@/components/ui/tooltip";
import { Thread } from "@/components/thread.aui";
import {
  APIError,
  archiveSession,
  deleteSession,
  getModels,
  getProjectsAll,
  getState,
  unarchiveSession,
} from "./api";
import { useSaiAdapter } from "./runtime";
import { SessionStore } from "./store";
import { convertRecords } from "./convert";
import {
  archiveBlockedHint,
  deleteBlockedHint,
  opStep,
  stateProbeHint,
  type ModelDescriptor,
  type OpStep,
} from "./newsession";
import { NewSessionDialog } from "./components/new-session-dialog";
import { ConfirmDeleteDialog } from "./components/confirm-delete-dialog";
import { ApprovalCard } from "./components/approval-card";
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
        // The all view (§17.3): archived rows arrive flagged and the
        // sidebar partitions them — one poll serves both sections.
        const p = await getProjectsAll();
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
  // ---- local draft entries (C6a-fix2, design §17.2) ----
  // A created-but-never-messaged session is NOT in the server listing
  // (the §17.8-e trade-off) — the client shows it locally until the
  // first message materializes it (the poll's row then supersedes the
  // draft). In-memory only: a reload or an eviction loses it, exactly
  // the accepted semantics.
  const [drafts, setDrafts] = useState<{ id: string; project: string }[]>([]);
  // ---- the sidebar tree (C6a-fix2) ----
  // Collapsed projects persist in localStorage; the default is
  // EXPANDED (discoverability first — a fresh serve shows everything,
  // collapsing is opt-in).
  const [collapsed, setCollapsed] = useState<Set<string>>(() => {
    try {
      return new Set(JSON.parse(localStorage.getItem("sai.collapsedProjects") || "[]"));
    } catch {
      return new Set();
    }
  });
  const toggleProject = useCallback((id: string) => {
    setCollapsed(prev => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      try {
        localStorage.setItem("sai.collapsedProjects", JSON.stringify([...next]));
      } catch {
        /* a full or blocked localStorage keeps the state in-memory */
      }
      return next;
    });
  }, []);
  // Which project's archived subnode is expanded (per visit, not
  // persisted — the archived tree is a detour, not a destination).
  const [archivedOpenIn, setArchivedOpenIn] = useState<string | null>(null);
  // The per-project "+" preselects its project in the dialog.
  const [newPreset, setNewPreset] = useState<string | null>(null);
  const openNewIn = useCallback((projectId: string) => {
    setNewPreset(projectId);
    setNewOpen(true);
  }, []);
  const onCreated = useCallback((id: string, project: string) => {
    setDrafts(prev => (prev.some(d => d.id === id) ? prev : [...prev, { id, project }]));
    setSessionID(id);
  }, [setSessionID]);
  const currentProject = useMemo(() => {
    for (const p of projects?.projects || []) {
      if (p.sessions.some((sess) => sess.id === sessionID)) return p.id;
    }
    return projects?.projects[0]?.id ?? "";
  }, [projects, sessionID]);

  // ---- delete confirmation (C6a) ----
  const [deleteTarget, setDeleteTarget] = useState<{ id: string; title: string } | null>(null);
  const requestDelete = useCallback((id: string, title: string) => {
    setOpHint("");
    setDeleteTarget({ id, title });
  }, []);
  // runOp is the shared delete/archive flow (opStep drives it): the
  // busy flag spans the WHOLE loop — set before the first attempt,
  // cleared only at done or hint — so a retry in flight cannot be
  // double-clicked into a second concurrent loop.
  const runOp = useCallback(
    async (
      send: () => Promise<unknown>,
      blockedHint: (e: { status: number; code: string }) => string | null,
      onDone: () => void,
    ) => {
      setOpBusy(true);
      try {
        for (let attempt = 0; ; attempt++) {
          let e: { status: number; code: string } | null = null;
          try {
            await send();
          } catch (err) {
            e = err instanceof APIError
              ? { status: err.status, code: err.code }
              : { status: 0, code: "" };
          }
          const step: OpStep = opStep(
            attempt,
            10,
            e,
            blockedHint,
            // Retry exhaustion means the closing window did NOT
            // self-heal: someone else holds the session open (another
            // tab, another client) — say so instead of "try again".
            "the session is open elsewhere — close it there first",
            "the operation did not go through — try again in a moment",
          );
          if (step.kind === "done") {
            onDone();
            return;
          }
          if (step.kind === "retry") {
            await new Promise((res) => setTimeout(res, 500));
            continue;
          }
          setOpHint(step.hint);
          return;
        }
      } finally {
        setOpBusy(false);
      }
    },
    [],
  );

  const confirmDelete = useCallback(async () => {
    if (!deleteTarget) return;
    const id = deleteTarget.id;
    // Deleting the open session first navigates away (the stream
    // holds server-side refs → session_open); bounded retries cover
    // the closing window.
    if (id === sessionID) setSessionID("");
    await runOp(
      () => deleteSession(id),
      deleteBlockedHint,
      () => {
        onDeleted(id);
        setDeleteTarget(null);
      },
    );
  }, [deleteTarget, sessionID, setSessionID, onDeleted, runOp]);

  // ---- archive / restore (C6b) ----
  // Archiving is reversible, so no confirmation dialog: the current
  // session navigates away first (same detach-then-act shape as the
  // delete), the parked row reappears in the collapsed section when
  // the poll refreshes.
  const [opHint, setOpHint] = useState("");
  const [opBusy, setOpBusy] = useState(false);
  const archive = useCallback(
    (id: string) => {
      setOpHint("");
      void (async () => {
        if (id === sessionID) setSessionID("");
        await runOp(
          () => archiveSession(id),
          archiveBlockedHint,
          () => setOpHint(""),
        );
      })();
    },
    [sessionID, setSessionID, runOp],
  );
  const restore = useCallback(
    (id: string) => {
      setOpHint("");
      void runOp(() => unarchiveSession(id), archiveBlockedHint, () => setOpHint(""));
    },
    [runOp],
  );

  // ---- the approval card's transient hints (C6c) ----
  const [approvalHint, setApprovalHint] = useState("");

  // ---- deep-link probe (the log view's unknown-id handling) ----
  // The SSE stream cannot read status codes; a deep link to a deleted
  // or unknown session would otherwise sit as a silent empty view.
  // Probe /state first: 404 presents the hint and bounces to the
  // list; a locked session presents its conflict and stays.
  const [routeHint, setRouteHint] = useState("");
  useEffect(() => {
    if (!sessionID) return;
    let cancelled = false;
    getState(sessionID)
      .then(() => setRouteHint(""))
      .catch((e) => {
        if (cancelled) return;
        const err = e instanceof APIError ? { status: e.status, code: e.code } : null;
        const hint = err ? stateProbeHint(err) : null;
        if (err?.status === 404) {
          setRouteHint(hint ?? "session not found");
          setSessionID("");
        } else if (hint) {
          setRouteHint(hint);
        }
      });
    return () => {
      cancelled = true;
    };
  }, [sessionID, setSessionID]);

  const adapter = useSaiAdapter(stores, sessionID, projects, setSessionID, onDeleted, openNew, requestDelete);
  const runtime = useExternalStoreRuntime(adapter);

  // The status strip reads the current session's snapshot directly —
  // the same store bumps that re-render the thread re-render this.
  const store = stores.get(sessionID);
  const snap = useSyncExternalStore(
    store ? store.subscribe : () => () => {},
    store ? store.getSnapshot : () => null,
  );

  // The sidebar tree (C6a-fix2): project nodes with their session
  // children. Draft rows ride inside their project until the listing
  // takes over.
  const projectNodes = useMemo(() => {
    const nodes: {
      id: string;
      path: string;
      live: { id: string; title: string; path: string }[];
      archived: { id: string; title: string; path: string }[];
      drafts: string[];
    }[] = [];
    for (const p of projects?.projects || []) {
      const live: { id: string; title: string; path: string }[] = [];
      const archived: { id: string; title: string; path: string }[] = [];
      for (const s of p.sessions) {
        (s.archived ? archived : live).push({
          id: s.id,
          title: s.title || "untitled",
          path: p.path,
        });
      }
      const listed = new Set([...live, ...archived].map(s => s.id));
      nodes.push({
        id: p.id,
        path: p.path,
        live,
        archived,
          // The draft carries the project id straight from the dialog —
        // no client-side path→id derivation (that hash is the
        // server's business).
        drafts: drafts.filter(d => d.project === p.id && !listed.has(d.id)).map(d => d.id),
      });
    }
    return nodes;
  }, [projects, drafts]);

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
              <span className="flex items-center gap-2">
                <button
                  className="hover:text-foreground cursor-pointer border-none bg-transparent text-base leading-none"
                  onClick={openNew}
                  aria-label="new session"
                  title="new session (also: type /new in the composer)"
                >
                  +
                </button>
                <a
                  className="hover:text-foreground no-underline"
                  href="/log"
                  title="the record-level log view"
                >
                  /log
                </a>
              </span>
            </div>
            <div className="flex flex-col gap-1 overflow-y-auto p-2">
              {projectNodes.map(node => {
                const open = !collapsed.has(node.id);
                // basename: strip trailing separators, then the last
                // path segment — BOTH separators, because a Windows
                // path of only backslashes would survive a
                // forward-slash-only split and show the full path.
                const label = node.path.replace(/[\\/]+$/, "").split(/[\\/]/).pop();
                return (
                  <div key={node.id} data-project={node.id}>
                    <div
                      className="group flex h-10 items-center gap-1 rounded-lg px-2 hover:bg-accent/50"
                      title={node.path}
                    >
                      <button
                        className="text-muted-foreground hover:text-foreground flex h-full w-5 shrink-0 cursor-pointer items-center justify-center border-none bg-transparent text-xs"
                        onClick={() => toggleProject(node.id)}
                        aria-expanded={open}
                        aria-label={open ? "collapse project " + label : "expand project " + label}
                      >
                        {open ? "▾" : "▸"}
                      </button>
                      <button
                        className="text-foreground flex h-full min-w-0 flex-1 cursor-pointer items-center truncate border-none bg-transparent text-left text-sm font-medium"
                        onClick={() => toggleProject(node.id)}
                      >
                        {label}
                      </button>
                      <button
                        className="text-muted-foreground hover:text-foreground invisible h-6 w-6 shrink-0 cursor-pointer border-none bg-transparent text-base leading-none group-hover:visible"
                        onClick={() => openNewIn(node.id)}
                        aria-label={"new session in " + label}
                        title={"new session in this project"}
                      >
                        +
                      </button>
                    </div>
                    {open && (
                      <div className="ml-3 flex flex-col gap-0.5 border-l border-border pl-1">
                        {node.live.map(sess => (
                          <div
                            key={sess.id}
                            className={
                              "group flex h-8 items-center gap-0.5 rounded-md font-sans text-sm " +
                              (sess.id === sessionID ? "bg-accent text-accent-foreground" : "hover:bg-accent/50")
                            }
                          >
                            <button
                              className="flex h-full min-w-0 flex-1 cursor-pointer items-center truncate border-none bg-transparent px-2 text-left"
                              onClick={() => setSessionID(sess.id)}
                              title={sess.title}
                            >
                              <span className="truncate">{sess.title}</span>
                            </button>
                            <span className="flex h-full items-center pr-1">
                              <button
                                className="text-muted-foreground hover:text-foreground invisible cursor-pointer border-none bg-transparent px-1 text-xs group-hover:visible"
                                disabled={opBusy}
                                onClick={e => {
                                  e.stopPropagation();
                                  archive(sess.id);
                                }}
                                aria-label={"archive session " + sess.title}
                                title="park this session out of the working set"
                              >
                                ⌄
                              </button>
                              <button
                                className="text-muted-foreground hover:text-destructive invisible cursor-pointer border-none bg-transparent px-1 text-xs group-hover:visible"
                                onClick={e => {
                                  e.stopPropagation();
                                  requestDelete(sess.id, sess.title);
                                }}
                                aria-label={"delete session " + sess.title}
                                title="delete this session"
                              >
                                ×
                              </button>
                            </span>
                          </div>
                        ))}
                        {node.drafts.map(id => (
                          <div
                            key={id}
                            className={
                              "group flex h-8 items-center rounded-md font-sans text-sm " +
                              (id === sessionID ? "bg-accent text-accent-foreground" : "hover:bg-accent/50")
                            }
                          >
                            <button
                              className="flex h-full min-w-0 flex-1 cursor-pointer items-center truncate border-none bg-transparent px-2 text-left"
                              onClick={() => setSessionID(id)}
                              title="created, not yet messaged — the listing takes over with the first message"
                            >
                              <span className="text-muted-foreground truncate italic">new session (draft)</span>
                            </button>
                          </div>
                        ))}
                        {node.live.length === 0 && node.drafts.length === 0 && (
                          <div className="text-muted-foreground flex h-8 items-center px-2 text-sm">
                            no sessions
                          </div>
                        )}
                        {node.archived.length > 0 && (
                          <div>
                            <button
                              className="text-muted-foreground hover:text-foreground flex h-7 w-full cursor-pointer items-center border-none bg-transparent px-2 text-left text-xs uppercase tracking-wider"
                              onClick={() =>
                                setArchivedOpenIn(archivedOpenIn === node.id ? null : node.id)
                              }
                              aria-expanded={archivedOpenIn === node.id}
                            >
                              archived ({node.archived.length}) {archivedOpenIn === node.id ? "▾" : "▸"}
                            </button>
                            {archivedOpenIn === node.id &&
                              node.archived.map(sess => (
                                <button
                                  key={sess.id}
                                  className="text-muted-foreground flex h-8 w-full cursor-pointer items-center truncate rounded-md border-none bg-transparent px-2 text-left font-sans text-sm hover:bg-accent/50"
                                  disabled={opBusy}
                                  onClick={() => restore(sess.id)}
                                  title="restore this session to the working set"
                                >
                                  <span className="truncate">{sess.title} · click to restore</span>
                                </button>
                              ))}
                          </div>
                        )}
                      </div>
                    )}
                  </div>
                );
              })}
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
              {routeHint && <span className="text-destructive">{routeHint}</span>}
              {opHint && <span className="text-destructive">{opHint}</span>}
              {approvalHint && <span className="text-accent-foreground">{approvalHint}</span>}
            </div>
            <div className="aui-root relative flex min-h-0 flex-1 flex-col items-center justify-start">
              {snap?.approval && sessionID && (
                <ApprovalCard
                  sessionID={sessionID}
                  approval={snap.approval}
                  onCollapse={() => store?.collapseApproval()}
                  onHint={hint => setApprovalHint(hint)}
                />
              )}
              <Thread
                autoFocus
                placeholder={
                  sessionID
                    ? undefined
                    : "No session — create one with the + above, or type /new once it exists"
                }
              />
            </div>
          </div>
        </div>
        <NewSessionDialog
          open={newOpen}
          onClose={() => {
            setNewOpen(false);
            setNewPreset(null);
          }}
          projects={(projects?.projects ?? []).map((p) => ({ id: p.id, path: p.path }))}
          models={models ?? []}
          currentProject={newPreset ?? currentProject}
          onCreated={onCreated}
        />
        <ConfirmDeleteDialog
          target={deleteTarget}
          onCancel={() => setDeleteTarget(null)}
          onConfirm={() => void confirmDelete()}
          hint={opHint}
          busy={opBusy}
        />
      </AssistantRuntimeProvider>
    </TooltipProvider>
  );
}
