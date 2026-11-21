import { Component, useCallback, useEffect, useMemo, useRef, useState, type ErrorInfo, type ReactNode } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Pencil } from "lucide-react";
import { api, onUnauthorized, setCSRFToken } from "./api";
import { useEventStream, useHotkeys, useMediaQuery, usePersistentState } from "./hooks";
import { keys, useLabels, useMailboxes } from "./queries";
import { navigate, useLocation } from "./router";
import { SessionProvider, useSession, type Session } from "./session";
import type { ChangeEvent, Label, Mailbox, SessionInfo, Settings, User } from "./types";
import { displayName } from "./util";
import { mailHref, parseRoute, viewTitles, type Route } from "./views";
import { AdminPage } from "./components/Admin";
import { LoginPage, SetupPage } from "./components/Auth";
import { Spinner } from "./components/common";
import { ComposerProvider, useComposer } from "./components/Composer";
import { ContactsPage } from "./components/Contacts";
import { DialogProvider } from "./components/Dialog";
import { MailPage } from "./components/MailPage";
import { SettingsPage } from "./components/Settings";
import { ShortcutsHelp } from "./components/Shortcuts";
import { Sidebar } from "./components/Sidebar";
import { ToastProvider } from "./components/Toasts";
import { TopBar } from "./components/TopBar";

async function loadSession(): Promise<SessionInfo> {
  const s = await api.session();
  setCSRFToken(s.csrf_token ?? "");
  return s;
}

export function App() {
  const qc = useQueryClient();
  const session = useQuery({ queryKey: keys.session, queryFn: loadSession, staleTime: Infinity, retry: 2 });

  /** Shows the sign-in page and forgets the previous user's data. */
  const signedOut = useCallback(() => {
    setCSRFToken("");
    qc.setQueryData<SessionInfo>(keys.session, (old) =>
      old ? { ...old, authenticated: false, user: undefined, settings: undefined, csrf_token: undefined, addresses: undefined } : old,
    );
    qc.removeQueries({ predicate: (q) => q.queryKey[0] !== keys.session[0] });
    void qc.invalidateQueries({ queryKey: keys.session });
  }, [qc]);

  useEffect(() => onUnauthorized(signedOut), [signedOut]);

  const signedIn = useCallback(
    (s: SessionInfo) => {
      qc.removeQueries({ predicate: (q) => q.queryKey[0] !== keys.session[0] });
      setCSRFToken(s.csrf_token ?? "");
      qc.setQueryData(keys.session, s);
      // Leave /setup?token=… so the token doesn't stay in the address bar or history.
      if (location.pathname === "/setup" || location.pathname === "/login") navigate("/mail/inbox", { replace: true });
    },
    [qc],
  );

  if (session.isPending) return <Spinner label="Loading TitoMail…" />;
  if (session.isError) {
    return (
      <div className="fatal">
        <h1>Can't reach TitoMail</h1>
        <p>{session.error.message}</p>
        <button className="btn primary" onClick={() => void session.refetch()}>
          Try again
        </button>
      </div>
    );
  }
  const info = session.data;
  if (!info.authenticated || !info.user || !info.settings) {
    return info.setup_required ? <SetupPage info={info} onSignedIn={signedIn} /> : <LoginPage info={info} onSignedIn={signedIn} />;
  }
  return (
    <ToastProvider>
      <DialogProvider>
        <SignedIn info={info} user={info.user} settings={info.settings} onSignedOut={signedOut} />
      </DialogProvider>
    </ToastProvider>
  );
}

function SignedIn({ info, user, settings, onSignedOut }: { info: SessionInfo; user: User; settings: Settings; onSignedOut: () => void }) {
  const qc = useQueryClient();

  const saveSettings = useCallback(
    async (patch: Partial<Settings>) => {
      const current = qc.getQueryData<SessionInfo>(keys.session)?.settings ?? settings;
      const saved = await api.saveSettings({ ...current, ...patch });
      qc.setQueryData<SessionInfo>(keys.session, (old) => (old ? { ...old, settings: saved } : old));
      // Settings such as conversation view and page size change list results.
      void qc.invalidateQueries({ queryKey: keys.conversations });
      return saved;
    },
    [qc, settings],
  );

  const setUser = useCallback(
    (u: User) =>
      qc.setQueryData<SessionInfo>(keys.session, (old) =>
        old?.user ? { ...old, user: { ...u, used_bytes: u.used_bytes || old.user.used_bytes } } : old,
      ),
    [qc],
  );

  const logout = useCallback(async () => {
    try {
      await api.logout();
    } catch {
      // The session is forgotten locally either way.
    }
    onSignedOut();
  }, [onSignedOut]);

  const value = useMemo<Session>(() => {
    const addresses = info.addresses?.length ? info.addresses : [user.email];
    return {
      info,
      user,
      settings,
      addresses,
      mine: new Set(addresses.map((a) => a.toLowerCase())),
      saveSettings,
      setUser,
      logout,
    };
  }, [info, user, settings, saveSettings, setUser, logout]);

  return (
    <SessionProvider value={value}>
      <ComposerProvider>
        <ErrorBoundary>
          <Shell onSignedOut={onSignedOut} />
        </ErrorBoundary>
      </ComposerProvider>
    </SessionProvider>
  );
}

const notificationsSupported = typeof Notification !== "undefined";

function Shell({ onSignedOut }: { onSignedOut: () => void }) {
  const { user, settings } = useSession();
  const loc = useLocation();
  const route = useMemo(() => parseRoute(loc), [loc]);
  const qc = useQueryClient();
  const composer = useComposer();
  const { mailboxes } = useMailboxes();
  const { labels } = useLabels();
  const narrow = useMediaQuery("(max-width: 800px)");
  const [collapsed, setCollapsed] = usePersistentState("sidebar-collapsed", false);
  const [drawer, setDrawer] = useState(false);
  const [shortcuts, setShortcuts] = useState(false);

  useEffect(() => {
    if (route.section === "redirect") navigate(route.to, { replace: true });
  }, [route]);

  useEffect(() => {
    if (!narrow) setDrawer(false);
  }, [narrow]);

  useEffect(() => {
    const root = document.documentElement;
    root.dataset.theme = settings.theme;
    root.dataset.density = settings.density;
  }, [settings.theme, settings.density]);

  const inbox = mailboxes.find((m) => m.special_use === "inbox");
  const title = pageTitle(route, mailboxes, labels);
  useEffect(() => {
    document.title = `${title} - ${user.email} - TitoMail`;
  }, [title, user.email]);

  useHotkeys({
    c: () => composer.compose(),
    "?": () => setShortcuts(true),
    "g i": () => navigate("/mail/inbox"),
    "g s": () => navigate("/mail/starred"),
    "g t": () => navigate("/mail/sent"),
    "g d": () => navigate("/mail/drafts"),
    "g a": () => navigate("/mail/all"),
    "g c": () => navigate("/contacts"),
  });

  const notify = useNewMailNotifications(inbox?.id, settings);

  useEventStream((ev: ChangeEvent) => {
    if (ev.mailboxes) {
      void qc.invalidateQueries({ queryKey: keys.mailboxes });
      void qc.invalidateQueries({ queryKey: keys.labels });
    }
    if (ev.messages) {
      void qc.invalidateQueries({ queryKey: keys.conversations });
      void qc.invalidateQueries({ queryKey: keys.thread });
      void qc.invalidateQueries({ queryKey: keys.message });
      void qc.invalidateQueries({ queryKey: keys.outbox });
    }
    if (ev.added?.length) notify(ev.added);
  }, onSignedOut);

  let content: ReactNode = null;
  switch (route.section) {
    case "mail":
      content = <MailPage route={route} />;
      break;
    case "settings":
      content = <SettingsPage tab={route.tab} />;
      break;
    case "contacts":
      content = <ContactsPage />;
      break;
    case "admin":
      content = <AdminPage tab={route.tab} />;
      break;
  }

  const toggleSidebar = () => (narrow ? setDrawer(!drawer) : setCollapsed(!collapsed));

  return (
    <div className="app">
      <a className="skip-link" href="#main">
        Skip to content
      </a>
      <TopBar
        query={route.section === "mail" && route.view.name === "search" ? route.q : ""}
        onToggleSidebar={toggleSidebar}
        onShowShortcuts={() => setShortcuts(true)}
      />
      <div className="app-body">
        <div className={"sidebar-wrap" + (narrow ? " drawer" : "") + (drawer ? " open" : "")}>
          <Sidebar
            current={route.section === "mail" ? route.view : null}
            collapsed={!narrow && collapsed}
            onNavigate={() => setDrawer(false)}
          />
        </div>
        {narrow && drawer && <div className="scrim" onClick={() => setDrawer(false)} aria-hidden="true" />}
        <main id="main" className="app-main" tabIndex={-1}>
          <ErrorBoundary resetKey={loc.path}>{content}</ErrorBoundary>
        </main>
      </div>
      {narrow && route.section === "mail" && !route.open && (
        <button className="compose-btn compose-fab" onClick={() => composer.compose()}>
          <Pencil size={20} />
          <span>Compose</span>
        </button>
      )}
      {shortcuts && <ShortcutsHelp onClose={() => setShortcuts(false)} />}
    </div>
  );
}

function pageTitle(route: Route, mailboxes: Mailbox[], labels: Label[]): string {
  switch (route.section) {
    case "settings":
      return "Settings";
    case "contacts":
      return "Contacts";
    case "admin":
      return "Administration";
    case "redirect":
      return "TitoMail";
  }
  const v = route.view;
  const count = (n: number | undefined) => (n ? ` (${n})` : "");
  if (v.name === "folder") {
    const mb = mailboxes.find((m) => m.id === v.id);
    return (mb?.name ?? "Folder") + count(mb?.unread);
  }
  if (v.name === "label") {
    const l = labels.find((x) => x.id === v.id);
    return (l?.name ?? "Label") + count(l?.unread);
  }
  if (v.name === "inbox") return "Inbox" + count(mailboxes.find((m) => m.special_use === "inbox")?.unread);
  if (v.name === "spam") return "Spam" + count(mailboxes.find((m) => m.special_use === "junk")?.unread);
  if (v.name === "search" && route.q) return `Search results for ${route.q}`;
  return viewTitles[v.name];
}

/**
 * Shows desktop notifications for new unread Inbox messages while the
 * window is in the background. Returns the handler for "added" events.
 */
function useNewMailNotifications(inboxId: number | undefined, settings: Settings) {
  const enabled = notificationsSupported && settings.desktop_notifications && inboxId !== undefined;
  // Messages with an ID above this have not been notified yet; -1 until the
  // current Inbox has been looked at, so existing mail never notifies.
  const seen = useRef(-1);
  const busy = useRef(false);
  const again = useRef(false);
  const conversationView = settings.conversation_view;

  const fetchUnread = useCallback(async () => {
    const params = new URLSearchParams({ view: "mailbox", mailbox: String(inboxId), q: "is:unread", threaded: "0", limit: "10" });
    return (await api.conversations(params)).conversations ?? [];
  }, [inboxId]);

  useEffect(() => {
    seen.current = -1;
    if (!enabled) return;
    let cancelled = false;
    // An empty Inbox has no messages, so use the newest ID of all mail.
    void Promise.all([fetchUnread(), api.conversations(new URLSearchParams({ view: "all", threaded: "0", limit: "1" }))])
      .then(([unread, all]) => {
        if (cancelled) return;
        const ids = [...unread, ...(all.conversations ?? [])].map((c) => c.latest_id);
        seen.current = Math.max(0, ...ids);
      })
      .catch(() => {
        // Notifications stay off until the next reload.
      });
    return () => {
      cancelled = true;
    };
  }, [enabled, fetchUnread]);

  const check = useRef<() => void>(() => {});
  check.current = () => {
    if (!enabled || seen.current < 0) return;
    if (busy.current) {
      again.current = true;
      return;
    }
    busy.current = true;
    void fetchUnread()
      .then((list) => {
        const fresh = list.filter((c) => c.latest_id > seen.current);
        if (!fresh.length) return;
        seen.current = Math.max(seen.current, ...fresh.map((c) => c.latest_id));
        if (Notification.permission !== "granted" || (!document.hidden && document.hasFocus())) return;
        const show = (title: string, body: string, tag: string, to: string) => {
          const n = new Notification(title, { body, tag, icon: "/favicon.svg" });
          n.onclick = () => {
            window.focus();
            navigate(to);
            n.close();
          };
        };
        if (fresh.length > 3) {
          show(`${fresh.length} new messages`, "TitoMail", "titomail-new", "/mail/inbox");
          return;
        }
        for (const c of fresh) {
          const from = c.participants?.[0];
          const open = conversationView ? ({ type: "thread", id: c.thread_id } as const) : ({ type: "message", id: c.latest_id } as const);
          show(
            from ? displayName(from) : "New message",
            [c.subject || "(no subject)", c.snippet].filter(Boolean).join("\n"),
            `titomail-${c.latest_id}`,
            mailHref({ name: "inbox" }, { open }),
          );
        }
      })
      .catch(() => {
        // Missing a notification is harmless.
      })
      .finally(() => {
        busy.current = false;
        if (again.current) {
          again.current = false;
          check.current();
        }
      });
  };

  return useCallback(
    (added: number[]) => {
      if (inboxId !== undefined && added.includes(inboxId)) check.current();
    },
    [inboxId],
  );
}

/** Catches rendering errors; with a resetKey, it tries again when the key changes (for example after navigating). */
class ErrorBoundary extends Component<{ children: ReactNode; resetKey?: string }, { error: Error | null }> {
  state = { error: null as Error | null };

  static getDerivedStateFromError(error: Error) {
    return { error };
  }

  componentDidUpdate(prev: { resetKey?: string }) {
    if (this.state.error && prev.resetKey !== this.props.resetKey) this.setState({ error: null });
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error("TitoMail crashed:", error, info.componentStack);
  }

  render() {
    if (!this.state.error) return this.props.children;
    return (
      <div className="fatal" role="alert">
        <h1>Something went wrong</h1>
        <p>TitoMail ran into a problem showing this page. Your mail is safe.</p>
        <pre className="muted small">{this.state.error.message}</pre>
        <button className="btn primary" onClick={() => location.reload()}>
          Reload
        </button>
      </div>
    );
  }
}
