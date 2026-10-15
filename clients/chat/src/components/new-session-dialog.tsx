// The new-session dialog (C6a, §17.6): project + model + (conditional)
// reasoning level, no first-message field — the created session starts
// empty and the composer takes the first message. State decisions live
// in ../newsession.ts (node-tested); this file is the rendering.
import { useEffect, useMemo, useState } from "react";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { APIError, createSession } from "../api";
import {
  createSessionHint,
  defaultModel,
  reasoningLevelsOf,
  type ModelDescriptor,
} from "../newsession";

export function NewSessionDialog({
  open,
  onClose,
  projects,
  models,
  currentProject,
  onCreated,
}: {
  open: boolean;
  onClose: () => void;
  projects: { id: string; path: string }[];
  models: ModelDescriptor[];
  currentProject: string;
  onCreated: (id: string) => void;
}) {
  // Defaults re-derive each time the dialog OPENS: the projects poll
  // and the models fetch are outside this component's lifetime, and a
  // selection made in a previous opening is not a "default" anymore.
  const [project, setProject] = useState("");
  const [provider, setProvider] = useState("");
  const [model, setModel] = useState("");
  const [reasoning, setReasoning] = useState("");
  const [hint, setHint] = useState("");
  const [submitting, setSubmitting] = useState(false);

  const preset = useMemo(() => defaultModel(models), [models]);
  useEffect(() => {
    if (!open) return;
    setProject(
      projects.find((p) => p.id === currentProject)?.id ??
        projects[0]?.id ??
        "",
    );
    setProvider(preset?.provider ?? "");
    setModel(preset?.model ?? "");
    setReasoning("");
    setHint("");
    setSubmitting(false);
  }, [open, projects, currentProject, preset]);

  const levels = useMemo(
    () => reasoningLevelsOf(models, provider, model),
    [models, provider, model],
  );
  // A model switch invalidates the picked tier (tiers differ per
  // model); "" is always valid — it means the model's default.
  useEffect(() => {
    if (reasoning && !levels.includes(reasoning)) setReasoning("");
  }, [levels, reasoning]);

  const submit = async () => {
    if (!project || !provider || !model || submitting) return;
    setSubmitting(true);
    setHint("");
    try {
      const created = await createSession(project, {
        provider,
        model,
        reasoning_level: reasoning,
      });
      onCreated(created.id);
      onClose();
    } catch (e) {
      const err =
        e instanceof APIError
          ? { status: e.status, code: e.code }
          : { status: 0, code: "" };
      setHint(createSessionHint(err));
      setSubmitting(false); // stay open — the user can fix and retry
    }
  };

  const selectClass =
    "border-border bg-background w-full rounded-lg border px-2 py-1.5 text-sm outline-none";

  return (
    <Dialog open={open} onOpenChange={(o) => !o && onClose()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>New session</DialogTitle>
          <DialogDescription>
            Created empty — the first message goes in the composer.
          </DialogDescription>
        </DialogHeader>
        {projects.length === 0 || models.length === 0 ? (
          <div className="text-muted-foreground text-sm">
            {projects.length === 0
              ? "no registered projects — add one from the CLI (sai project add)"
              : "no models configured — check the providers in sai.yaml"}
          </div>
        ) : (
          <div className="flex flex-col gap-3">
            <label className="flex flex-col gap-1 text-xs">
              project
              <select
                className={selectClass}
                value={project}
                onChange={(e) => setProject(e.target.value)}
              >
                {projects.map((p) => (
                  <option key={p.id} value={p.id}>
                    {p.path.replace(/[\\/]+$/, "").split(/[\\/]/).pop()}
                  </option>
                ))}
              </select>
            </label>
            <label className="flex flex-col gap-1 text-xs">
              model
              <select
                className={selectClass}
                value={provider + "/" + model}
                onChange={(e) => {
                  const [p, m] = e.target.value.split("/");
                  setProvider(p);
                  setModel(m);
                }}
              >
                {models.map((m) => (
                  <option key={m.provider + "/" + m.model} value={m.provider + "/" + m.model}>
                    {m.provider}/{m.model}
                    {m.default ? " (default)" : ""}
                  </option>
                ))}
              </select>
            </label>
            {levels.length > 0 && (
              <label className="flex flex-col gap-1 text-xs">
                reasoning
                <select
                  className={selectClass}
                  value={reasoning}
                  onChange={(e) => setReasoning(e.target.value)}
                >
                  <option value="">model default</option>
                  {levels.map((lvl) => (
                    <option key={lvl} value={lvl}>
                      {lvl}
                    </option>
                  ))}
                </select>
              </label>
            )}
            {hint && <div className="text-destructive text-xs">{hint}</div>}
          </div>
        )}
        <DialogFooter>
          <Button variant="outline" onClick={onClose} disabled={submitting}>
            Cancel
          </Button>
          <Button
            onClick={() => void submit()}
            disabled={!project || !provider || !model || submitting}
          >
            {submitting ? "creating…" : "Create"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
