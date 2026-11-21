import { createContext, useContext } from "react";
import type { SessionInfo, Settings, User } from "./types";

export interface Session {
  info: SessionInfo;
  user: User;
  settings: Settings;
  /** The user's own addresses (lowercase), including aliases. */
  mine: Set<string>;
  addresses: string[];
  saveSettings: (patch: Partial<Settings>) => Promise<Settings>;
  setUser: (u: User) => void;
  logout: () => Promise<void>;
}

const SessionContext = createContext<Session | null>(null);

export const SessionProvider = SessionContext.Provider;

export function useSession(): Session {
  const s = useContext(SessionContext);
  if (!s) throw new Error("useSession outside of SessionProvider");
  return s;
}
