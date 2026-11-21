import { useSyncExternalStore, type AnchorHTMLAttributes, type MouseEvent } from "react";

// A tiny router on top of the History API.

const listeners = new Set<() => void>();

function notify() {
  for (const l of listeners) l();
}

window.addEventListener("popstate", notify);

export function navigate(to: string, opts: { replace?: boolean } = {}) {
  if (to === location.pathname + location.search) return;
  if (opts.replace) history.replaceState(null, "", to);
  else history.pushState(null, "", to);
  notify();
}

function subscribe(fn: () => void) {
  listeners.add(fn);
  return () => {
    listeners.delete(fn);
  };
}

const snapshot = () => location.pathname + location.search;

export interface Location {
  path: string;
  segments: string[];
  query: URLSearchParams;
  href: string;
}

let cachedHref = "";
let cachedLocation: Location | null = null;

export function useLocation(): Location {
  const href = useSyncExternalStore(subscribe, snapshot);
  if (href !== cachedHref || !cachedLocation) {
    const url = new URL(href, location.origin);
    cachedHref = href;
    cachedLocation = {
      path: url.pathname,
      segments: url.pathname.split("/").filter(Boolean).map(decodeURIComponent),
      query: url.searchParams,
      href,
    };
  }
  return cachedLocation;
}

type LinkProps = AnchorHTMLAttributes<HTMLAnchorElement> & { to: string; replace?: boolean };

export function Link({ to, replace, onClick, ...rest }: LinkProps) {
  const handle = (e: MouseEvent<HTMLAnchorElement>) => {
    onClick?.(e);
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    e.preventDefault();
    navigate(to, { replace });
  };
  return <a href={to} onClick={handle} {...rest} />;
}
