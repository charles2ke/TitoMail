import { useEffect, useRef, useState } from "react";

interface Props {
  document: string;
  showQuotes: boolean;
  onMailto?: (address: string, subject: string) => void;
}

const maxHeight = 30000;

/**
 * Shows a sanitized message in a sandboxed iframe: scripts can't run, and
 * the document's own CSP blocks remote content unless the user allowed it.
 * The frame grows to fit its content so the page scrolls as one.
 */
export function MessageBody({ document: doc, showQuotes, onMailto }: Props) {
  const ref = useRef<HTMLIFrameElement>(null);
  const [height, setHeight] = useState(80);
  const cleanup = useRef<() => void>(() => {});
  const mailtoRef = useRef(onMailto);
  mailtoRef.current = onMailto;
  const quotesRef = useRef(showQuotes);
  quotesRef.current = showQuotes;

  const onLoad = () => {
    cleanup.current();
    const d = ref.current?.contentDocument;
    if (!d?.body) return;
    d.body.classList.toggle("tm-show-quotes", quotesRef.current);
    const measure = () => {
      const h = Math.max(d.body.scrollHeight, d.documentElement.offsetHeight);
      setHeight(Math.min(maxHeight, Math.ceil(h) + 2));
    };
    measure();
    const ro = new ResizeObserver(measure);
    ro.observe(d.body);
    ro.observe(d.documentElement);
    const onClick = (e: MouseEvent) => {
      const a = (e.target as Element | null)?.closest?.("a[href]") as HTMLAnchorElement | null;
      if (!a) return;
      const href = a.getAttribute("href") ?? "";
      if (href.toLowerCase().startsWith("mailto:") && mailtoRef.current) {
        e.preventDefault();
        try {
          const u = new URL(href);
          mailtoRef.current(decodeURIComponent(u.pathname), u.searchParams.get("subject") ?? "");
        } catch {
          mailtoRef.current(href.slice(7), "");
        }
      } else if (href.startsWith("#")) {
        e.preventDefault();
      }
    };
    d.addEventListener("click", onClick);
    cleanup.current = () => {
      ro.disconnect();
      d.removeEventListener("click", onClick);
    };
  };

  useEffect(() => () => cleanup.current(), []);

  useEffect(() => {
    ref.current?.contentDocument?.body?.classList.toggle("tm-show-quotes", showQuotes);
  }, [showQuotes]);

  return (
    <iframe
      ref={ref}
      className="msg-frame"
      title="Message"
      sandbox="allow-same-origin allow-popups allow-popups-to-escape-sandbox"
      srcDoc={doc}
      onLoad={onLoad}
      style={{ height }}
    />
  );
}
