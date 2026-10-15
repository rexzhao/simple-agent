// The tool-approval card (C6c, §16.3): a pending tool call blocks on
// the host's decision — the card shows what wants to run and offers
// approve/deny (deny with an optional reason). State decisions live
// in ../newsession.ts (node-tested); this file is the rendering.
import { useState } from "react";
import { APIError, postApproval } from "../api";
import {
  approvalButtonsDisabled,
  approvalErrorAction,
  truncateArgs,
} from "../newsession";

export function ApprovalCard({
  sessionID,
  approval,
  onCollapse,
  onHint,
}: {
  sessionID: string;
  approval: { item: string; name: string; args: string; outcome: string | null };
  onCollapse: () => void;
  onHint: (hint: string) => void;
}) {
  const [submitting, setSubmitting] = useState(false);
  const [expanded, setExpanded] = useState(false);
  const [denying, setDenying] = useState(false);
  const [reason, setReason] = useState("");

  const disabled = approvalButtonsDisabled(submitting, approval.outcome);

  const submit = async (allow: boolean) => {
    if (disabled) return;
    setSubmitting(true);
    try {
      await postApproval(sessionID, approval.item, allow, allow ? undefined : reason);
      // The decision went through; the resolved EVENT collapses the
      // card into its outcome display — nothing to do here.
    } catch (e) {
      const err = e instanceof APIError ? { status: e.status, code: e.code } : { status: 0, code: "" };
      const action = approvalErrorAction(err);
      if (action.kind === "collapse") onCollapse();
      onHint(action.hint);
      // "wait": the card stays, the resolved event is coming — do
      // NOT re-enable the buttons (a resubmit would race).
      if (action.kind === "hint") setSubmitting(false);
    }
  };

  const argsPreview = truncateArgs(approval.args);
  const outcomeLine =
    approval.outcome === "approved"
      ? "approved — running"
      : approval.outcome === "denied"
        ? "denied"
        : approval.outcome === "cancelled"
          ? "cancelled — the run was stopped"
          : null;

  return (
    <div data-testid="approval-card" className="border-border bg-muted/30 mx-auto mb-2 w-full max-w-2xl rounded-lg border p-3 text-sm">
      <div className="flex items-baseline justify-between gap-2">
        <span className="font-medium">
          tool approval{approval.name ? `: ${approval.name}` : ""}
        </span>
        {outcomeLine ? (
          <span className="text-muted-foreground text-xs">{outcomeLine}</span>
        ) : (
          <span className="text-muted-foreground text-xs">waiting for your decision</span>
        )}
      </div>
      <pre className="bg-background mt-2 max-h-40 overflow-auto rounded p-2 text-xs whitespace-pre-wrap">
        {expanded || argsPreview === approval.args ? approval.args : argsPreview}
      </pre>
      {argsPreview !== approval.args && (
        <button
          className="text-muted-foreground hover:text-foreground cursor-pointer border-none bg-transparent p-0 text-xs underline"
          onClick={() => setExpanded(!expanded)}
        >
          {expanded ? "show less" : "show all arguments"}
        </button>
      )}
      {!approval.outcome && (
        <div className="mt-2 flex items-center gap-2">
          {denying && (
            <input
              className="border-border bg-background flex-1 rounded border px-2 py-1 text-xs"
              placeholder="reason (optional)"
              value={reason}
              onChange={(e) => setReason(e.target.value)}
            />
          )}
          <button
            className="cursor-pointer rounded-lg border border-transparent bg-primary px-3 py-1 text-xs text-primary-foreground disabled:pointer-events-none disabled:opacity-50"
            disabled={disabled}
            onClick={() => void submit(true)}
          >
            approve
          </button>
          <button
            className="destructive cursor-pointer rounded-lg border border-transparent px-3 py-1 text-xs disabled:pointer-events-none disabled:opacity-50"
            disabled={disabled}
            onClick={() => (denying ? void submit(false) : setDenying(true))}
          >
            {denying ? "confirm deny" : "deny…"}
          </button>
        </div>
      )}
    </div>
  );
}
