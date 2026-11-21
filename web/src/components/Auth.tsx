import { useState, type FormEvent, type ReactNode } from "react";
import { Mail } from "lucide-react";
import { api } from "../api";
import { useLocation } from "../router";
import type { SessionInfo } from "../types";

const minPassword = 8;

function errorMessage(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

function AuthCard({ title, subtitle, children }: { title: string; subtitle?: ReactNode; children: ReactNode }) {
  return (
    <main className="auth-page">
      <div className="auth-card">
        <div className="auth-logo">
          <Mail size={32} aria-hidden="true" />
          <span>TitoMail</span>
        </div>
        <h1>{title}</h1>
        {subtitle && <p className="muted">{subtitle}</p>}
        {children}
      </div>
    </main>
  );
}

export function LoginPage({ info, onSignedIn }: { info: SessionInfo; onSignedIn: (s: SessionInfo) => void }) {
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setBusy(true);
    try {
      onSignedIn(await api.login(email.trim(), password));
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  return (
    <AuthCard title="Sign in" subtitle={info.hostname ? `to ${info.hostname}` : undefined}>
      <form className="stack" onSubmit={submit}>
        <label className="field">
          <span>Email</span>
          <input
            type="email"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            autoComplete="username"
            autoCapitalize="off"
            spellCheck={false}
            required
            autoFocus
          />
        </label>
        <label className="field">
          <span>Password</span>
          <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="current-password" required />
        </label>
        {error && (
          <div className="error-text" role="alert">
            {error}
          </div>
        )}
        <button className="btn primary block" type="submit" disabled={busy}>
          {busy ? "Signing in…" : "Sign in"}
        </button>
      </form>
    </AuthCard>
  );
}

export function SetupPage({ info, onSignedIn }: { info: SessionInfo; onSignedIn: (s: SessionInfo) => void }) {
  const loc = useLocation();
  const linkToken = loc.query.get("token") ?? "";
  const [token, setToken] = useState(linkToken);
  const [name, setName] = useState("");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    if (password.length < minPassword) {
      setError(`Use at least ${minPassword} characters for the password.`);
      return;
    }
    if (password !== confirm) {
      setError("The passwords don't match.");
      return;
    }
    setBusy(true);
    try {
      onSignedIn(await api.setup({ token: token.trim(), email: email.trim(), password, display_name: name.trim() }));
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  return (
    <AuthCard
      title="Set up your mail server"
      subtitle={<>Create the first account on {info.hostname || "this server"}. It will be an administrator.</>}
    >
      <form className="stack" onSubmit={submit}>
        {!linkToken && (
          <label className="field">
            <span>Setup token</span>
            <input value={token} onChange={(e) => setToken(e.target.value)} autoComplete="off" spellCheck={false} required autoFocus />
            <span className="muted small">Copy it from the server log, or open the setup link printed there.</span>
          </label>
        )}
        <label className="field">
          <span>Your name</span>
          <input value={name} onChange={(e) => setName(e.target.value)} autoComplete="name" maxLength={200} autoFocus={!!linkToken} />
        </label>
        <label className="field">
          <span>Email address</span>
          <input
            type="email"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            autoComplete="username"
            autoCapitalize="off"
            spellCheck={false}
            placeholder="you@example.com"
            required
          />
          <span className="muted small">Its domain is added to the server automatically.</span>
        </label>
        <label className="field">
          <span>Password</span>
          <input
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            autoComplete="new-password"
            minLength={minPassword}
            maxLength={72}
            required
          />
        </label>
        <label className="field">
          <span>Confirm password</span>
          <input type="password" value={confirm} onChange={(e) => setConfirm(e.target.value)} autoComplete="new-password" required />
        </label>
        {error && (
          <div className="error-text" role="alert">
            {error}
          </div>
        )}
        <button className="btn primary block" type="submit" disabled={busy}>
          {busy ? "Creating…" : "Create account"}
        </button>
      </form>
    </AuthCard>
  );
}
