// The render-check entry: real records -> real conversion -> REAL
// assistant-ui Thread, rendered server-side. Built with
// `vite build --ssr` by TestRenderFixture and driven by
// internal/web/testdata/render_check.mjs — a browser-free pin on the
// render layer, including the adapter SHAPE (useExternalStoreRuntime
// takes the adapter directly; wrapping it as {adapter} has been
// written twice by mistake and only this layer catches it).
import React from "react";
import { renderToReadableStream } from "react-dom/server";
import { AssistantRuntimeProvider, useExternalStoreRuntime } from "@assistant-ui/react";
import { Thread } from "./components/thread.aui";
import { TooltipProvider } from "./components/ui/tooltip";
import { convertRecords, toThreadMessageLike } from "./convert";
import type { Frame } from "./types";

async function snapshot(base: string, sid: string): Promise<Frame> {
  const res = await fetch(`${base}/api/s/${sid}/stream`);
  const reader = res.body!.getReader();
  const dec = new TextDecoder();
  let buf = "";
  for (;;) {
    const { done, value } = await reader.read();
    if (done) throw new Error("stream ended before a snapshot frame");
    buf += dec.decode(value, { stream: true });
    const idx = buf.indexOf("\n\n");
    if (idx > 0) {
      await reader.cancel();
      const line = buf.slice(0, idx);
      if (!line.startsWith("data: ")) throw new Error("not a data line");
      return JSON.parse(line.slice(6));
    }
  }
}

export async function renderProbe(base: string, sid: string) {
  const frame = await snapshot(base, sid);
  const messages = convertRecords(frame.records || []);

  function Probe() {
    // The adapter is passed DIRECTLY — the shape this check exists to
    // pin.
    const runtime = useExternalStoreRuntime({
      isRunning: false,
      messages,
      convertMessage: toThreadMessageLike,
      onNew: async () => {},
      adapters: { threadList: { threadId: sid, threads: [{ status: "regular" as const, id: sid, title: "probe" }] } },
    });
    return React.createElement(
      TooltipProvider,
      null,
      React.createElement(AssistantRuntimeProvider, { runtime }, React.createElement(Thread, null)),
    );
  }
  const stream = await renderToReadableStream(React.createElement(Probe));
  const html = await new Response(stream).text();
  return {
    records: (frame.records || []).length,
    messages: messages.length,
    htmlLength: html.length,
    html,
  };
}
