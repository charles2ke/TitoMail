import { useState, type ReactNode } from "react";
import { Archive, EllipsisVertical, FolderInput, Inbox, Mail, MailOpen, OctagonAlert, ShieldCheck, Star, StarOff, Tag, Trash2 } from "lucide-react";
import { api, type ActionBody } from "../api";
import { useMailActions, type Target } from "../actions";
import { useLabels, useMailboxes } from "../queries";
import type { Label } from "../types";
import type { ResolvedView } from "../views";
import { useDialogs } from "./Dialog";
import { LabelEditor } from "./LabelEditor";
import { Menu, Popover } from "./Menu";
import { LabelPicker, MovePicker, type LabelState } from "./Pickers";
import { useToast } from "./Toasts";

export interface ActionDef {
  body: ActionBody;
  label: string;
  icon: ReactNode;
  /** Keyboard shortcut that runs this action. */
  key?: string;
}

const archive: ActionDef = { body: { action: "archive" }, label: "Archive", icon: <Archive size={20} />, key: "e" };
const spam: ActionDef = { body: { action: "spam" }, label: "Report spam", icon: <OctagonAlert size={20} />, key: "!" };
const trash: ActionDef = { body: { action: "trash" }, label: "Delete", icon: <Trash2 size={20} />, key: "#" };
const toInbox: ActionDef = { body: { action: "inbox" }, label: "Move to Inbox", icon: <Inbox size={20} /> };
const notSpam: ActionDef = { body: { action: "notspam" }, label: "Not spam", icon: <ShieldCheck size={20} />, key: "!" };
const forever: ActionDef = { body: { action: "delete" }, label: "Delete forever", icon: <Trash2 size={20} />, key: "#" };

/** The main actions offered for conversations in a view, like Gmail's toolbar. */
export function viewActions(v: ResolvedView): ActionDef[] {
  switch (v.special) {
    case "inbox":
      return [archive, spam, trash];
    case "archive":
      return [toInbox, spam, trash];
    case "sent":
      return [trash];
    case "drafts":
      return [{ ...forever, label: "Discard drafts" }];
    case "junk":
      return [notSpam, forever];
    case "trash":
      return [toInbox, forever];
  }
  if (v.mailbox) return [archive, toInbox, spam, trash];
  return [archive, spam, trash];
}

interface ActionBarProps {
  view: ResolvedView;
  target: Target;
  anyUnread: boolean;
  anyStarred: boolean;
  labelState: (labelId: number) => LabelState;
  /** Called after an action that moved the conversations out of the way. */
  onMoved?: () => void;
  /** Called after "Mark as unread". */
  onMarkedUnread?: () => void;
}

export function ActionBar({ view, target, anyUnread, anyStarred, labelState, onMoved, onMarkedUnread }: ActionBarProps) {
  const { perform } = useMailActions();
  const { mailboxes } = useMailboxes();
  const { labels } = useLabels();
  const dialogs = useDialogs();
  const toast = useToast();
  const [creatingLabel, setCreatingLabel] = useState(false);

  const run = async (def: ActionDef) => {
    if (await perform(target, def.body)) onMoved?.();
  };
  const moveTo = async (mailboxId: number, name: string) => {
    if (await perform(target, { action: "move", mailbox_id: mailboxId }, { name })) onMoved?.();
  };
  const createFolder = async () => {
    const name = await dialogs.prompt({ title: "New folder", label: "Folder name", confirmLabel: "Create" });
    if (!name) return;
    try {
      const mb = await api.createMailbox(name);
      await moveTo(mb.id, mb.name);
    } catch (err) {
      toast.error(err);
    }
  };
  const toggleLabel = (l: Label, add: boolean) =>
    void perform(target, { action: add ? "label" : "unlabel", label_id: l.id }, { name: l.name });

  return (
    <div className="action-bar">
      {viewActions(view).map((def) => (
        <button key={def.body.action} className="icon-btn" title={def.label} aria-label={def.label} onClick={() => void run(def)}>
          {def.icon}
        </button>
      ))}
      <span className="sep" />
      {anyUnread ? (
        <button
          className="icon-btn"
          title="Mark as read"
          aria-label="Mark as read"
          onClick={() => void perform(target, { action: "read" })}
        >
          <MailOpen size={20} />
        </button>
      ) : (
        <button
          className="icon-btn"
          title="Mark as unread"
          aria-label="Mark as unread"
          onClick={() => void perform(target, { action: "unread" }).then((ok) => ok && onMarkedUnread?.())}
        >
          <Mail size={20} />
        </button>
      )}
      {view.special !== "drafts" && (
        <>
          <Popover
            role="menu"
            className="menu-popover"
            trigger={(p) => (
              <button className="icon-btn" title="Move to" aria-label="Move to" {...p}>
                <FolderInput size={20} />
              </button>
            )}
          >
            {(close) => (
              <MovePicker
                mailboxes={mailboxes}
                currentId={view.mailbox?.id}
                onPick={(m, name) => {
                  close();
                  void moveTo(m.id, name);
                }}
                onCreate={() => {
                  close();
                  void createFolder();
                }}
              />
            )}
          </Popover>
          <Popover
            role="menu"
            className="menu-popover"
            trigger={(p) => (
              <button className="icon-btn" title="Labels" aria-label="Labels" {...p}>
                <Tag size={20} />
              </button>
            )}
          >
            {(close) => (
              <LabelPicker
                labels={labels}
                stateOf={labelState}
                onToggle={toggleLabel}
                onCreate={() => {
                  close();
                  setCreatingLabel(true);
                }}
              />
            )}
          </Popover>
        </>
      )}
      <Menu
        trigger={(p) => (
          <button className="icon-btn" title="More" aria-label="More actions" {...p}>
            <EllipsisVertical size={20} />
          </button>
        )}
        items={[
          anyUnread
            ? { label: "Mark as unread", icon: <Mail size={16} />, onSelect: () => void perform(target, { action: "unread" }).then((ok) => ok && onMarkedUnread?.()) }
            : { label: "Mark as read", icon: <MailOpen size={16} />, onSelect: () => void perform(target, { action: "read" }) },
          { label: "Add star", icon: <Star size={16} />, onSelect: () => void perform(target, { action: "star" }) },
          anyStarred && { label: "Remove star", icon: <StarOff size={16} />, onSelect: () => void perform(target, { action: "unstar" }) },
          view.special !== "junk" && view.special !== "drafts" && { label: "Report spam", icon: <OctagonAlert size={16} />, onSelect: () => void run(spam) },
          view.special !== "trash" && view.special !== "junk" && view.special !== "drafts" && {
            label: "Delete forever",
            icon: <Trash2 size={16} />,
            danger: true,
            onSelect: () => void run(forever),
          },
        ]}
      />
      {creatingLabel && (
        <LabelEditor label={null} onClose={() => setCreatingLabel(false)} onCreated={(l) => toggleLabel(l, true)} />
      )}
    </div>
  );
}
