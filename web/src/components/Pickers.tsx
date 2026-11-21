import { useMemo, useState, type KeyboardEvent } from "react";
import { FolderPlus, Plus } from "lucide-react";
import type { Label, Mailbox, SpecialUse } from "../types";
import { folderTree } from "../views";
import { onMenuKey } from "./Menu";

const specialNames: [SpecialUse, string][] = [
  ["inbox", "Inbox"],
  ["archive", "Archive"],
  ["junk", "Spam"],
  ["trash", "Trash"],
];

function focusFirstItem(e: KeyboardEvent<HTMLInputElement>) {
  if (e.key !== "ArrowDown") return;
  e.preventDefault();
  e.currentTarget.closest(".picker")?.querySelector<HTMLElement>("[role=menuitem], input[type=checkbox]")?.focus();
}

interface MovePickerProps {
  mailboxes: Mailbox[];
  currentId?: number;
  onPick: (m: Mailbox, name: string) => void;
  onCreate: () => void;
}

/** "Move to" menu with a filter field, listing the system folders and the user's folders. */
export function MovePicker({ mailboxes, currentId, onPick, onCreate }: MovePickerProps) {
  const [filter, setFilter] = useState("");
  const items = useMemo(() => {
    const out: { mailbox: Mailbox; name: string }[] = [];
    for (const [s, name] of specialNames) {
      const m = mailboxes.find((x) => x.special_use === s);
      if (m) out.push({ mailbox: m, name });
    }
    for (const f of folderTree(mailboxes)) out.push({ mailbox: f.mailbox, name: f.mailbox.name });
    return out.filter((x) => x.mailbox.id !== currentId);
  }, [mailboxes, currentId]);
  const q = filter.trim().toLowerCase();
  const shown = q ? items.filter((x) => x.name.toLowerCase().includes(q)) : items;
  return (
    <div className="picker" onKeyDown={onMenuKey}>
      <div className="menu-heading">Move to:</div>
      <input
        className="picker-filter"
        placeholder="Search folders"
        aria-label="Search folders"
        autoFocus
        value={filter}
        onChange={(e) => setFilter(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "Enter" && shown.length > 0) {
            e.preventDefault();
            onPick(shown[0].mailbox, shown[0].name);
          } else focusFirstItem(e);
        }}
      />
      <div className="picker-list">
        {shown.map((x) => (
          <button key={x.mailbox.id} type="button" role="menuitem" className="menu-item" onClick={() => onPick(x.mailbox, x.name)}>
            <span className="menu-label">{x.name}</span>
          </button>
        ))}
        {shown.length === 0 && <div className="picker-empty">No matching folders</div>}
      </div>
      <div className="menu-sep" />
      <button type="button" role="menuitem" className="menu-item" onClick={onCreate}>
        <span className="menu-icon">
          <FolderPlus size={16} />
        </span>
        <span className="menu-label">Create new folder</span>
      </button>
    </div>
  );
}

export type LabelState = "all" | "some" | "none";

interface LabelPickerProps {
  labels: Label[];
  stateOf: (labelId: number) => LabelState;
  onToggle: (l: Label, add: boolean) => void;
  onCreate: () => void;
}

/** "Label as" menu: each checkbox adds or removes a label right away. */
export function LabelPicker({ labels, stateOf, onToggle, onCreate }: LabelPickerProps) {
  const [filter, setFilter] = useState("");
  const [overrides, setOverrides] = useState<Record<number, boolean>>({});
  const q = filter.trim().toLowerCase();
  const shown = q ? labels.filter((l) => l.name.toLowerCase().includes(q)) : labels;
  const state = (id: number): LabelState => (id in overrides ? (overrides[id] ? "all" : "none") : stateOf(id));
  return (
    <div className="picker">
      <div className="menu-heading">Label as:</div>
      <input
        className="picker-filter"
        placeholder="Search labels"
        aria-label="Search labels"
        autoFocus
        value={filter}
        onChange={(e) => setFilter(e.target.value)}
        onKeyDown={focusFirstItem}
      />
      <div className="picker-list">
        {shown.map((l) => {
          const s = state(l.id);
          return (
            <label key={l.id} className="picker-check">
              <input
                type="checkbox"
                checked={s === "all"}
                ref={(el) => {
                  if (el) el.indeterminate = s === "some";
                }}
                onChange={() => {
                  const add = s !== "all";
                  setOverrides({ ...overrides, [l.id]: add });
                  onToggle(l, add);
                }}
              />
              <span className="label-dot" style={l.color ? { background: l.color } : undefined} />
              <span className="menu-label">{l.name}</span>
            </label>
          );
        })}
        {shown.length === 0 && <div className="picker-empty">{labels.length ? "No matching labels" : "No labels yet"}</div>}
      </div>
      <div className="menu-sep" />
      <button type="button" role="menuitem" className="menu-item" onClick={onCreate}>
        <span className="menu-icon">
          <Plus size={16} />
        </span>
        <span className="menu-label">Create new label</span>
      </button>
    </div>
  );
}
