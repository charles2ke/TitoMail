import { useEffect, useRef, useState, type RefObject } from "react";
import { api } from "./api";
import type { ChangeEvent } from "./types";

/** Returns true when the event target is a text field, where hotkeys must not fire. */
export function isTyping(e: Event): boolean {
  const t = e.target as HTMLElement | null;
  if (!t) return false;
  const tag = t.tagName;
  return tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT" || t.isContentEditable;
}

export type HotkeyMap = Record<string, (e: KeyboardEvent) => void>;

type HotkeyRef = { current: HotkeyMap };

// All active hotkey maps share one listener so that a sequence prefix like
// "g" is consumed once, instead of also firing a single-key binding ("g a"
// must not trigger another component's "a").
const activeMaps: HotkeyRef[] = [];
let prefix = "";
let prefixTimer = 0;

function onHotkey(e: KeyboardEvent) {
  if (e.defaultPrevented || e.ctrlKey || e.metaKey || e.altKey || isTyping(e)) return;
  if (document.querySelector("[data-modal-open]")) return;
  let key = e.key.length === 1 ? e.key.toLowerCase() : e.key;
  if (e.shiftKey && /^[a-z]$/.test(key)) key = "shift+" + key;
  const maps = activeMaps.map((r) => r.current);
  const run = (k: string) => {
    const fn = maps.find((m) => m[k])?.[k];
    if (fn) {
      e.preventDefault();
      fn(e);
    }
  };
  if (prefix) {
    const seq = `${prefix} ${key}`;
    prefix = "";
    window.clearTimeout(prefixTimer);
    run(seq);
    return;
  }
  if (maps.some((m) => Object.keys(m).some((k) => k.startsWith(key + " ")))) {
    prefix = key;
    prefixTimer = window.setTimeout(() => (prefix = ""), 1500);
    return;
  }
  run(key);
}

/**
 * Binds single keys ("j", "#", "shift+i", "?") and two-key sequences ("g i")
 * like Gmail. Handlers always see the latest map without re-binding. When
 * several components bind the same key, the first one mounted wins.
 */
export function useHotkeys(map: HotkeyMap, enabled = true) {
  const ref = useRef(map);
  ref.current = map;
  useEffect(() => {
    if (!enabled) return;
    if (activeMaps.length === 0) document.addEventListener("keydown", onHotkey);
    activeMaps.push(ref);
    return () => {
      activeMaps.splice(activeMaps.indexOf(ref), 1);
      if (activeMaps.length === 0) {
        document.removeEventListener("keydown", onHotkey);
        prefix = "";
        window.clearTimeout(prefixTimer);
      }
    };
  }, [enabled]);
}

/**
 * Subscribes to the server's live change notifications. EventSource retries
 * by itself after network errors; when the server rejects the stream (for
 * example because the session expired) the session is checked again.
 */
export function useEventStream(onChange: (ev: ChangeEvent) => void, onLogout: () => void) {
  const changeRef = useRef(onChange);
  const logoutRef = useRef(onLogout);
  changeRef.current = onChange;
  logoutRef.current = onLogout;
  useEffect(() => {
    let es: EventSource | null = null;
    let timer = 0;
    let stopped = false;
    const connect = () => {
      es = new EventSource("/api/events");
      es.addEventListener("change", (e) => {
        try {
          changeRef.current(JSON.parse((e as MessageEvent).data));
        } catch {
          // Ignore malformed events.
        }
      });
      es.addEventListener("logout", () => {
        es?.close();
        logoutRef.current();
      });
      es.onerror = () => {
        if (es?.readyState !== EventSource.CLOSED || stopped) return;
        timer = window.setTimeout(async () => {
          try {
            const s = await api.session();
            if (!s.authenticated) {
              logoutRef.current();
              return;
            }
          } catch {
            // Offline: try again below.
          }
          if (!stopped) connect();
        }, 5000);
      };
    };
    connect();
    return () => {
      stopped = true;
      window.clearTimeout(timer);
      es?.close();
    };
  }, []);
}

export function useDebounced<T>(value: T, ms: number): T {
  const [v, setV] = useState(value);
  useEffect(() => {
    const t = window.setTimeout(() => setV(value), ms);
    return () => window.clearTimeout(t);
  }, [value, ms]);
  return v;
}

export function useMediaQuery(query: string): boolean {
  const [matches, setMatches] = useState(() => window.matchMedia(query).matches);
  useEffect(() => {
    const mq = window.matchMedia(query);
    const fn = () => setMatches(mq.matches);
    fn();
    mq.addEventListener("change", fn);
    return () => mq.removeEventListener("change", fn);
  }, [query]);
  return matches;
}

/** Calls fn when the user clicks outside of the referenced element or presses Escape. */
export function useDismiss(ref: RefObject<HTMLElement | null>, open: boolean, fn: () => void) {
  const fnRef = useRef(fn);
  fnRef.current = fn;
  useEffect(() => {
    if (!open) return;
    const onDown = (e: PointerEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) fnRef.current();
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.stopPropagation();
        fnRef.current();
      }
    };
    document.addEventListener("pointerdown", onDown, true);
    document.addEventListener("keydown", onKey, true);
    return () => {
      document.removeEventListener("pointerdown", onDown, true);
      document.removeEventListener("keydown", onKey, true);
    };
  }, [open, ref]);
}

export function usePersistentState<T>(key: string, initial: T): [T, (v: T) => void] {
  const [value, setValue] = useState<T>(() => {
    try {
      const s = localStorage.getItem(key);
      return s === null ? initial : (JSON.parse(s) as T);
    } catch {
      return initial;
    }
  });
  const set = (v: T) => {
    setValue(v);
    try {
      localStorage.setItem(key, JSON.stringify(v));
    } catch {
      // Storage may be unavailable (private mode); keep the in-memory value.
    }
  };
  return [value, set];
}
