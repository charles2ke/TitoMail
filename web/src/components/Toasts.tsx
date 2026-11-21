import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { X } from "lucide-react";

export interface ToastAction {
  label: string;
  run: () => void;
}

export interface ToastOptions {
  message: ReactNode;
  actions?: ToastAction[];
  /** Milliseconds before the toast hides; 0 keeps it until dismissed. */
  duration?: number;
  error?: boolean;
}

interface ToastItem extends ToastOptions {
  id: number;
}

interface ToastAPI {
  show: (t: ToastOptions) => number;
  update: (id: number, t: ToastOptions) => void;
  dismiss: (id: number) => void;
  error: (err: unknown) => void;
}

const ToastContext = createContext<ToastAPI | null>(null);

export function useToast(): ToastAPI {
  const t = useContext(ToastContext);
  if (!t) throw new Error("useToast outside of ToastProvider");
  return t;
}

let nextId = 1;

export function ToastProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<ToastItem[]>([]);
  const timers = useRef(new Map<number, number>());

  const dismiss = useCallback((id: number) => {
    window.clearTimeout(timers.current.get(id));
    timers.current.delete(id);
    setItems((list) => list.filter((t) => t.id !== id));
  }, []);

  const schedule = useCallback(
    (id: number, t: ToastOptions) => {
      window.clearTimeout(timers.current.get(id));
      const duration = t.duration ?? (t.error ? 8000 : 5000);
      if (duration > 0) timers.current.set(id, window.setTimeout(() => dismiss(id), duration));
    },
    [dismiss],
  );

  const show = useCallback(
    (t: ToastOptions) => {
      const id = nextId++;
      // Like Gmail, only the latest few notifications stay visible.
      setItems((list) => [...list.slice(-2), { ...t, id }]);
      schedule(id, t);
      return id;
    },
    [schedule],
  );

  const update = useCallback(
    (id: number, t: ToastOptions) => {
      setItems((list) => {
        if (!list.some((x) => x.id === id)) return [...list.slice(-2), { ...t, id }];
        return list.map((x) => (x.id === id ? { ...t, id } : x));
      });
      schedule(id, t);
    },
    [schedule],
  );

  const error = useCallback(
    (err: unknown) => {
      if (err instanceof DOMException && err.name === "AbortError") return;
      show({ message: err instanceof Error ? err.message : String(err), error: true });
    },
    [show],
  );

  useEffect(() => {
    const map = timers.current;
    return () => map.forEach((t) => window.clearTimeout(t));
  }, []);

  const value = useMemo(() => ({ show, update, dismiss, error }), [show, update, dismiss, error]);

  return (
    <ToastContext.Provider value={value}>
      {children}
      <div className="toasts" role="status" aria-live="polite">
        {items.map((t) => (
          <div key={t.id} className={"toast" + (t.error ? " error" : "")}>
            <span className="toast-msg">{t.message}</span>
            {t.actions?.map((a) => (
              <button
                key={a.label}
                className="toast-action"
                onClick={() => {
                  dismiss(t.id);
                  a.run();
                }}
              >
                {a.label}
              </button>
            ))}
            <button className="toast-close" aria-label="Dismiss" onClick={() => dismiss(t.id)}>
              <X size={16} />
            </button>
          </div>
        ))}
      </div>
    </ToastContext.Provider>
  );
}
