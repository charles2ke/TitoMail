import { useState, type FormEvent } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { Check } from "lucide-react";
import { api } from "../api";
import { keys } from "../queries";
import type { Label } from "../types";
import { Modal } from "./Dialog";
import { useToast } from "./Toasts";

export const labelColors = [
  "",
  "#d93025",
  "#e8710a",
  "#f9ab00",
  "#188038",
  "#12b5cb",
  "#1a73e8",
  "#9334e6",
  "#e52592",
  "#5f6368",
];

/** Dialog to create or edit a label. */
export function LabelEditor({ label, onClose, onCreated }: { label: Label | null; onClose: () => void; onCreated?: (l: Label) => void }) {
  const [name, setName] = useState(label?.name ?? "");
  const [color, setColor] = useState(label?.color ?? "");
  const [busy, setBusy] = useState(false);
  const qc = useQueryClient();
  const toast = useToast();

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (!name.trim()) return;
    setBusy(true);
    try {
      if (label) {
        await api.updateLabel(label.id, { name: name.trim(), color });
      } else {
        const l = await api.createLabel(name.trim(), color);
        onCreated?.(l);
      }
      void qc.invalidateQueries({ queryKey: keys.labels });
      onClose();
    } catch (err) {
      toast.error(err);
    } finally {
      setBusy(false);
    }
  };

  return (
    <Modal title={label ? "Edit label" : "New label"} onClose={onClose}>
      <form onSubmit={submit} className="dialog-form">
        <label className="field">
          <span>Label name</span>
          <input autoFocus value={name} onChange={(e) => setName(e.target.value)} maxLength={100} />
        </label>
        <div className="field">
          <span>Color</span>
          <div className="swatches" role="radiogroup" aria-label="Label color">
            {labelColors.map((c) => (
              <button
                key={c || "none"}
                type="button"
                role="radio"
                aria-checked={color === c}
                aria-label={c || "No color"}
                className={"swatch" + (c ? "" : " none")}
                style={c ? { background: c } : undefined}
                onClick={() => setColor(c)}
              >
                {color === c && <Check size={14} />}
              </button>
            ))}
          </div>
        </div>
        <div className="modal-foot inline">
          <button type="button" className="btn" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="btn primary" disabled={busy || !name.trim()}>
            {label ? "Save" : "Create"}
          </button>
        </div>
      </form>
    </Modal>
  );
}
