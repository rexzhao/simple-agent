// The delete confirmation dialog (C6a): deletion is irreversible and
// the sidebar's per-row button sits one misclick from it — the
// confirm shows WHAT dies and offers the way out. Cancelling sends
// nothing.
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";

export function ConfirmDeleteDialog({
  target,
  onCancel,
  onConfirm,
  hint,
  busy,
}: {
  target: { id: string; title: string } | null;
  onCancel: () => void;
  onConfirm: () => void;
  hint: string;
  busy: boolean;
}) {
  return (
    <Dialog open={target !== null} onOpenChange={(o) => !o && onCancel()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Delete session</DialogTitle>
          <DialogDescription>
            {target
              ? `“${target.title || "untitled"}” is deleted permanently — the records do not come back.`
              : ""}
          </DialogDescription>
        </DialogHeader>
        {hint && <div className="text-destructive text-xs">{hint}</div>}
        <DialogFooter>
          <Button variant="outline" onClick={onCancel} disabled={busy}>
            Cancel
          </Button>
          <Button variant="destructive" onClick={onConfirm} disabled={busy}>
            {busy ? "deleting…" : "Delete"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
