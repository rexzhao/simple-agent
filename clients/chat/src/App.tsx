import { useEffect, useMemo, useState } from "react";
import { useSyncExternalStore } from "react";
import {
  AssistantRuntimeProvider,
  useExternalStoreRuntime,
  MessagePrimitive,
  ComposerPrimitive,
  ThreadPrimitive,
  QueueItemPrimitive,
} from "@assistant-ui/react";
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

// ---- part renderers ----

function TextPartBody({ text }: { text: string }) {
  return <div className="part-text">{text}</div>;
}

function ReasoningPart({ text }: { text: string }) {
  return (
    <details className="part-reasoning">
      <summary>reasoning</summary>
      <div>{text}</div>
    </details>
  );
}

function ToolCallPart({
  toolName,
  argsText,
  result,
  isError,
}: {
  toolName: string;
  argsText?: string;
  result?: unknown;
  isError?: boolean;
}) {
  const args = argsText || "";
  const resultText = typeof result === "string" ? result : "";
  return (
    <div className={"part-tool" + (isError ? " err" : "")}>
      <div className="part-tool-head">
        {toolName}
        {resultText === "" ? " …" : ""}
      </div>
      {args && <pre className="part-tool-args">{args}</pre>}
      {resultText && <pre className="part-tool-result">{resultText}</pre>}
    </div>
  );
}

function MyMessage() {
  return (
    <MessagePrimitive.Root className="message">
      <MessagePrimitive.Parts
        components={{
          Text: TextPartBody,
          Reasoning: ReasoningPart,
          tools: { Fallback: ToolCallPart },
        }}
      />
    </MessagePrimitive.Root>
  );
}

// ---- the thread ----

function MyThread({ notice, verified }: { notice: string; verified: boolean | null }) {
  return (
    <ThreadPrimitive.Root className="thread">
      <div className="thread-status">
        <span className={"chain" + (verified === false ? " bad" : "")}>
          {verified === false ? "chain drift!" : verified ? "verified" : "…"}
        </span>
        {notice && <span className="notice">{notice}</span>}
      </div>
      <ThreadPrimitive.Viewport className="thread-viewport">
        <ThreadPrimitive.Empty>
          <div className="thread-empty">select a session from the list</div>
        </ThreadPrimitive.Empty>
        <ThreadPrimitive.Messages components={{ UserMessage: MyMessage, AssistantMessage: MyMessage }} />
        <ThreadPrimitive.ViewportFooter className="thread-footer">
          <ThreadPrimitive.LoadEarlier>load earlier messages</ThreadPrimitive.LoadEarlier>
          <ThreadPrimitive.ScrollToBottom>↓</ThreadPrimitive.ScrollToBottom>
        </ThreadPrimitive.ViewportFooter>
      </ThreadPrimitive.Viewport>
      <Composer />
    </ThreadPrimitive.Root>
  );
}

function Composer() {
  return (
    <ComposerPrimitive.Root className="composer">
      <ComposerPrimitive.Queue>
        {() => (
          <div className="queue-item">
            <QueueItemPrimitive.Text />
            <span className="queue-controls">
              <QueueItemPrimitive.Steer title="run now (stops the current run)">▲</QueueItemPrimitive.Steer>
              <QueueItemPrimitive.Remove title="withdraw">×</QueueItemPrimitive.Remove>
            </span>
          </div>
        )}
      </ComposerPrimitive.Queue>
      <ComposerPrimitive.Input placeholder="message…" autoFocus />
      <ComposerPrimitive.Send>send</ComposerPrimitive.Send>
      <ComposerPrimitive.Cancel>stop</ComposerPrimitive.Cancel>
    </ComposerPrimitive.Root>
  );
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
    if (store) void store.open(sessionID);
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
    const out: { id: string; title: string; project: string }[] = [];
    for (const p of projects?.projects || []) {
      for (const s of p.sessions) out.push({ id: s.id, title: s.title || "untitled", project: p.id });
    }
    return out;
  }, [projects]);

  return (
    <AssistantRuntimeProvider runtime={runtime}>
      <div id="layout">
        <nav id="sidebar">
          <div id="sidebar-head">
            sessions <a className="log-link" href="/log" title="the record-level log view">/log</a>
          </div>
          <div id="sessions">
            {sessions.length === 0 && <div className="empty">no sessions — create one from the CLI</div>}
            {sessions.map(s => (
              <button
                key={s.id}
                className={"session" + (s.id === sessionID ? " current" : "")}
                onClick={() => setSessionID(s.id)}
                title={s.project}
              >
                <span className="session-title">{s.title}</span>
                <span className="session-project">{s.project}</span>
              </button>
            ))}
          </div>
        </nav>
        <MyThread notice={snap?.notice || ""} verified={snap ? snap.verified : null} />
      </div>
    </AssistantRuntimeProvider>
  );
}
