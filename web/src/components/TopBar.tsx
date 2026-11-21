import { useEffect, useRef, useState, type FormEvent } from "react";
import { CircleHelp, LogOut, Menu as MenuIcon, Search, Settings, Shield, SlidersHorizontal, UserRound, BookUser, X } from "lucide-react";
import { navigate, Link } from "../router";
import { useSession } from "../session";
import { avatarColor, initials } from "../util";
import { Menu } from "./Menu";
import { Popover } from "./Menu";

interface TopBarProps {
  query: string;
  onToggleSidebar: () => void;
  onShowShortcuts: () => void;
}

function quoteValue(v: string) {
  const t = v.trim();
  return /\s/.test(t) ? `"${t.replace(/"/g, "")}"` : t;
}

function AdvancedSearch({ onSearch }: { onSearch: (q: string) => void }) {
  const [f, setF] = useState({ from: "", to: "", subject: "", words: "", without: "", attachment: false, within: "", unread: false });
  const set = (k: keyof typeof f) => (e: { target: { value: string; checked?: boolean; type?: string } }) =>
    setF({ ...f, [k]: e.target.type === "checkbox" ? e.target.checked : e.target.value });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    const parts: string[] = [];
    if (f.from.trim()) parts.push(`from:${quoteValue(f.from)}`);
    if (f.to.trim()) parts.push(`to:${quoteValue(f.to)}`);
    if (f.subject.trim()) parts.push(`subject:${quoteValue(f.subject)}`);
    if (f.words.trim()) parts.push(f.words.trim());
    for (const w of f.without.trim().split(/\s+/).filter(Boolean)) parts.push(`-${w}`);
    if (f.attachment) parts.push("has:attachment");
    if (f.unread) parts.push("is:unread");
    if (f.within) parts.push(`newer_than:${f.within}`);
    if (parts.length) onSearch(parts.join(" "));
  };
  return (
    <form className="adv-search" onSubmit={submit}>
      <label className="field row">
        <span>From</span>
        <input value={f.from} onChange={set("from")} autoFocus />
      </label>
      <label className="field row">
        <span>To</span>
        <input value={f.to} onChange={set("to")} />
      </label>
      <label className="field row">
        <span>Subject</span>
        <input value={f.subject} onChange={set("subject")} />
      </label>
      <label className="field row">
        <span>Has the words</span>
        <input value={f.words} onChange={set("words")} />
      </label>
      <label className="field row">
        <span>Doesn't have</span>
        <input value={f.without} onChange={set("without")} />
      </label>
      <label className="field row">
        <span>Date within</span>
        <select value={f.within} onChange={set("within")}>
          <option value="">Any time</option>
          <option value="1d">1 day</option>
          <option value="7d">1 week</option>
          <option value="1m">1 month</option>
          <option value="6m">6 months</option>
          <option value="1y">1 year</option>
        </select>
      </label>
      <div className="adv-checks">
        <label className="check">
          <input type="checkbox" checked={f.attachment} onChange={set("attachment")} /> Has attachment
        </label>
        <label className="check">
          <input type="checkbox" checked={f.unread} onChange={set("unread")} /> Unread only
        </label>
      </div>
      <div className="adv-foot">
        <span className="muted small">
          Tip: operators like <code>from:</code>, <code>is:unread</code>, <code>larger:5M</code> and <code>in:trash</code> also work in the search box.
        </span>
        <button className="btn primary" type="submit">
          Search
        </button>
      </div>
    </form>
  );
}

export function TopBar({ query, onToggleSidebar, onShowShortcuts }: TopBarProps) {
  const { user, logout } = useSession();
  const [text, setText] = useState(query);
  const inputRef = useRef<HTMLInputElement>(null);
  useEffect(() => setText(query), [query]);

  const search = (q: string) => {
    const t = q.trim();
    if (!t) return;
    setText(t);
    inputRef.current?.blur();
    navigate(`/mail/search?q=${encodeURIComponent(t)}`);
  };

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const t = e.target as HTMLElement;
      if (e.key !== "/" || e.ctrlKey || e.metaKey || e.altKey) return;
      if (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.isContentEditable || document.querySelector("[data-modal-open]")) return;
      e.preventDefault();
      inputRef.current?.focus();
      inputRef.current?.select();
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, []);

  const me = { name: user.display_name, address: user.email };
  return (
    <header className="topbar">
      <div className="topbar-left">
        <button className="icon-btn" aria-label="Main menu" onClick={onToggleSidebar}>
          <MenuIcon size={22} />
        </button>
        <Link to="/mail/inbox" className="brand" aria-label="TitoMail inbox">
          <img src="/favicon.svg" alt="" width={32} height={32} />
          <span>TitoMail</span>
        </Link>
      </div>
      <form
        className="search"
        role="search"
        onSubmit={(e) => {
          e.preventDefault();
          search(text);
        }}
      >
        <button className="icon-btn" type="submit" aria-label="Search">
          <Search size={20} />
        </button>
        <input
          ref={inputRef}
          value={text}
          onChange={(e) => setText(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Escape") {
              setText(query);
              inputRef.current?.blur();
            }
          }}
          placeholder="Search mail"
          aria-label="Search mail"
          spellCheck={false}
        />
        {text && (
          <button
            className="icon-btn"
            type="button"
            aria-label="Clear search"
            onClick={() => {
              setText("");
              if (query) navigate("/mail/inbox");
              inputRef.current?.focus();
            }}
          >
            <X size={18} />
          </button>
        )}
        <Popover
          align="right"
          className="adv-popover"
          trigger={(p) => (
            <button className="icon-btn" type="button" aria-label="Show search options" title="Show search options" {...p}>
              <SlidersHorizontal size={20} />
            </button>
          )}
        >
          {(close) => (
            <AdvancedSearch
              onSearch={(q) => {
                close();
                search(q);
              }}
            />
          )}
        </Popover>
      </form>
      <div className="topbar-right">
        <button className="icon-btn" aria-label="Keyboard shortcuts" title="Keyboard shortcuts (?)" onClick={onShowShortcuts}>
          <CircleHelp size={22} />
        </button>
        <Link to="/settings" className="icon-btn" aria-label="Settings" title="Settings">
          <Settings size={22} />
        </Link>
        <Menu
          align="right"
          trigger={(p) => (
            <button className="avatar-btn" aria-label={`Account: ${user.email}`} title={user.email} {...p}>
              <span className="avatar" style={{ background: avatarColor(user.email) }}>
                {initials(me)}
              </span>
            </button>
          )}
          items={[
            { heading: user.display_name ? `${user.display_name} · ${user.email}` : user.email },
            { label: "Manage your account", icon: <UserRound size={16} />, onSelect: () => navigate("/settings/account") },
            { label: "Contacts", icon: <BookUser size={16} />, onSelect: () => navigate("/contacts") },
            user.is_admin && { label: "Administration", icon: <Shield size={16} />, onSelect: () => navigate("/admin") },
            "separator",
            { label: "Sign out", icon: <LogOut size={16} />, onSelect: () => void logout() },
          ]}
        />
      </div>
    </header>
  );
}
