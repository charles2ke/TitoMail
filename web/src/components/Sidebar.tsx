import { useState, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import {
  Archive,
  Clock,
  EllipsisVertical,
  FileText,
  Folder,
  Inbox,
  Mails,
  OctagonAlert,
  Pencil,
  Plus,
  Send,
  Star,
  Tag,
  Trash2,
} from "lucide-react";
import { api } from "../api";
import { useFolderActions } from "../folderActions";
import { keys, useLabels, useMailboxes } from "../queries";
import { Link, navigate } from "../router";
import { useSession } from "../session";
import type { Label, Mailbox, SpecialUse } from "../types";
import { formatBytes, isDelayed, isScheduled } from "../util";
import { folderTree, mailHref, sameView, type SystemView, type ViewRef } from "../views";
import { useComposer } from "./Composer";
import { LabelEditor } from "./LabelEditor";
import { Menu } from "./Menu";

interface NavItem {
  view: SystemView;
  label: string;
  icon: ReactNode;
  special?: SpecialUse;
  count?: "unread" | "total";
}

const nav: NavItem[] = [
  { view: "inbox", label: "Inbox", icon: <Inbox size={20} />, special: "inbox", count: "unread" },
  { view: "starred", label: "Starred", icon: <Star size={20} /> },
  { view: "scheduled", label: "Scheduled", icon: <Clock size={20} /> },
  { view: "sent", label: "Sent", icon: <Send size={20} />, special: "sent" },
  { view: "drafts", label: "Drafts", icon: <FileText size={20} />, special: "drafts", count: "total" },
  { view: "archive", label: "Archive", icon: <Archive size={20} />, special: "archive" },
  { view: "all", label: "All mail", icon: <Mails size={20} /> },
  { view: "spam", label: "Spam", icon: <OctagonAlert size={20} />, special: "junk", count: "unread" },
  { view: "trash", label: "Trash", icon: <Trash2 size={20} />, special: "trash" },
];

interface SidebarProps {
  current: ViewRef | null;
  collapsed: boolean;
  onNavigate: () => void;
}

function NavLink(props: {
  to: string;
  active: boolean;
  icon: ReactNode;
  label: string;
  count?: number;
  bold?: boolean;
  depth?: number;
  menu?: ReactNode;
  onNavigate: () => void;
}) {
  return (
    <div className={"nav-item" + (props.active ? " active" : "") + (props.bold ? " bold" : "")}>
      <Link
        to={props.to}
        className="nav-link"
        title={props.label}
        style={props.depth ? { paddingLeft: 26 + props.depth * 14 } : undefined}
        aria-current={props.active ? "page" : undefined}
        onClick={props.onNavigate}
      >
        <span className="nav-icon">{props.icon}</span>
        <span className="nav-label">{props.label}</span>
        {!!props.count && <span className="nav-count">{props.count > 9999 ? "9999+" : props.count}</span>}
      </Link>
      {props.menu && <div className="nav-menu">{props.menu}</div>}
    </div>
  );
}

export function Sidebar({ current, collapsed, onNavigate }: SidebarProps) {
  const { user } = useSession();
  const composer = useComposer();
  const { mailboxes } = useMailboxes();
  const { labels } = useLabels();
  const folders = useFolderActions();
  const [editing, setEditing] = useState<Label | "new" | null>(null);
  const outbox = useQuery({ queryKey: keys.outbox, queryFn: api.outbox, staleTime: 30_000, refetchInterval: 60_000 });
  const scheduled = (outbox.data ?? []).filter((e) => isScheduled(e) || isDelayed(e)).length;

  const bySpecial = (s?: SpecialUse) => mailboxes.find((m) => m.special_use === s);

  const createFolder = async (parent?: Mailbox) => {
    const mb = await folders.createFolder(parent);
    if (mb) navigate(mailHref({ name: "folder", id: mb.id }));
  };

  const deleteFolder = async (mb: Mailbox) => {
    const deleted = await folders.deleteFolder(mb);
    // Subfolders are deleted too.
    const open = current?.name === "folder" ? mailboxes.find((m) => m.id === current.id) : undefined;
    if (deleted && open && (open.id === mb.id || open.name.startsWith(mb.name + "/"))) navigate("/mail/inbox");
  };

  const deleteLabel = async (l: Label) => {
    if ((await folders.deleteLabel(l)) && current?.name === "label" && current.id === l.id) navigate("/mail/inbox");
  };

  const quota = user.quota_bytes;
  // Mailbox sizes add up to the account's usage and stay current with live updates.
  const used = mailboxes.length ? mailboxes.reduce((n, m) => n + m.size, 0) : user.used_bytes;
  const pct = quota > 0 ? Math.min(100, (used / quota) * 100) : 0;

  return (
    <nav className={"sidebar" + (collapsed ? " collapsed" : "")} aria-label="Mailboxes">
      <div className="compose-wrap">
        <button className="compose-btn" onClick={() => composer.compose()} title="Compose (c)">
          <Pencil size={20} />
          <span>Compose</span>
        </button>
      </div>
      <div className="nav-section">
        {nav.map((item) => {
          const mb = bySpecial(item.special);
          const count =
            item.view === "scheduled" ? scheduled : item.count === "unread" ? mb?.unread : item.count === "total" ? mb?.total : undefined;
          if (item.view === "scheduled" && scheduled === 0 && current?.name !== "scheduled") return null;
          return (
            <NavLink
              key={item.view}
              to={mailHref({ name: item.view })}
              active={!!current && sameView(current, { name: item.view })}
              icon={item.icon}
              label={item.label}
              count={count}
              bold={item.count === "unread" && !!count}
              onNavigate={onNavigate}
              menu={
                (item.special === "trash" || item.special === "junk") &&
                mb &&
                mb.total > 0 && (
                  <Menu
                    align="right"
                    trigger={(p) => (
                      <button className="icon-btn small" aria-label={`${item.label} options`} {...p}>
                        <EllipsisVertical size={16} />
                      </button>
                    )}
                    items={[
                      {
                        label: `Empty ${item.label}`,
                        icon: <Trash2 size={16} />,
                        danger: true,
                        onSelect: () => void folders.emptyFolder(mb, item.label),
                      },
                    ]}
                  />
                )
              }
            />
          );
        })}
      </div>

      <div className="nav-heading">
        <span>Folders</span>
        <button className="icon-btn small" aria-label="Create folder" title="Create folder" onClick={() => void createFolder()}>
          <Plus size={18} />
        </button>
      </div>
      <div className="nav-section">
        {folderTree(mailboxes).map(({ mailbox: mb, depth, leaf }) => (
          <NavLink
            key={mb.id}
            to={mailHref({ name: "folder", id: mb.id })}
            active={current?.name === "folder" && current.id === mb.id}
            icon={<Folder size={18} />}
            label={leaf}
            count={mb.unread}
            bold={mb.unread > 0}
            depth={depth}
            onNavigate={onNavigate}
            menu={
              <Menu
                align="right"
                trigger={(p) => (
                  <button className="icon-btn small" aria-label={`Options for ${mb.name}`} {...p}>
                    <EllipsisVertical size={16} />
                  </button>
                )}
                items={[
                  { label: "Rename", onSelect: () => void folders.renameFolder(mb) },
                  { label: "New subfolder", onSelect: () => void createFolder(mb) },
                  "separator",
                  { label: "Delete", danger: true, onSelect: () => void deleteFolder(mb) },
                ]}
              />
            }
          />
        ))}
      </div>

      <div className="nav-heading">
        <span>Labels</span>
        <button className="icon-btn small" aria-label="Create label" title="Create label" onClick={() => setEditing("new")}>
          <Plus size={18} />
        </button>
      </div>
      <div className="nav-section">
        {labels.map((l) => (
          <NavLink
            key={l.id}
            to={mailHref({ name: "label", id: l.id })}
            active={current?.name === "label" && current.id === l.id}
            icon={<Tag size={18} fill={l.color || "none"} color={l.color || "currentColor"} />}
            label={l.name}
            count={l.unread}
            bold={l.unread > 0}
            onNavigate={onNavigate}
            menu={
              <Menu
                align="right"
                trigger={(p) => (
                  <button className="icon-btn small" aria-label={`Options for label ${l.name}`} {...p}>
                    <EllipsisVertical size={16} />
                  </button>
                )}
                items={[
                  { label: "Edit", onSelect: () => setEditing(l) },
                  { label: "Remove label", danger: true, onSelect: () => void deleteLabel(l) },
                ]}
              />
            }
          />
        ))}
      </div>

      <div className="storage" title={quota > 0 ? `${pct.toFixed(1)}% used` : undefined}>
        {quota > 0 && (
          <div className="storage-bar">
            <div style={{ width: `${pct}%` }} className={pct > 90 ? "full" : ""} />
          </div>
        )}
        <span>{quota > 0 ? `${formatBytes(used)} of ${formatBytes(quota)} used` : `${formatBytes(used)} used`}</span>
      </div>
      {editing && <LabelEditor label={editing === "new" ? null : editing} onClose={() => setEditing(null)} />}
    </nav>
  );
}
