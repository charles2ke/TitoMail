import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Clock, RotateCw } from "lucide-react";
import { api } from "../api";
import { keys } from "../queries";
import { navigate } from "../router";
import type { QueueEntry } from "../types";
import { fullDate, isDelayed, isScheduled } from "../util";
import { EmptyState, Spinner } from "./common";
import { useComposer } from "./Composer";
import { useToast } from "./Toasts";

/** Messages the user sent that are waiting in the outgoing queue (scheduled or being retried). */
export function Outbox() {
  const q = useQuery({ queryKey: keys.outbox, queryFn: api.outbox, refetchInterval: 30_000 });
  const qc = useQueryClient();
  const composer = useComposer();
  const toast = useToast();
  const entries = (q.data ?? []).filter((e) => isScheduled(e) || isDelayed(e));

  const cancel = async (e: QueueEntry) => {
    try {
      const r = await api.cancelSend(e.id);
      toast.show({ message: "Sending canceled. The message was moved to Drafts." });
      void composer.openDraft(r.draft_id);
    } catch (err) {
      toast.error(err);
    }
    void qc.invalidateQueries({ queryKey: keys.outbox });
    void qc.invalidateQueries({ queryKey: keys.conversations });
    void qc.invalidateQueries({ queryKey: keys.mailboxes });
  };

  return (
    <div className="mail-view pane-none">
      <section className="list-pane" aria-label="Scheduled">
        <div className="toolbar list-toolbar">
          <h2 className="toolbar-title">Scheduled</h2>
          <button className="icon-btn" title="Refresh" aria-label="Refresh" onClick={() => void q.refetch()}>
            <RotateCw size={18} className={q.isFetching ? "spin" : ""} />
          </button>
        </div>
        <div className="list-scroll">
          {q.isPending ? (
            <Spinner />
          ) : entries.length === 0 ? (
            <EmptyState icon={<Clock size={56} strokeWidth={1} />} title="Nothing scheduled">
              <p>Use the arrow next to Send to schedule a message for later.</p>
            </EmptyState>
          ) : (
            <div className="conv-list" role="list">
              {entries.map((e) => {
                const rcpts = e.recipients ?? [];
                const pending = rcpts.filter((r) => r.status === "pending");
                const next = pending.map((r) => r.next_attempt_at).sort()[0] ?? "";
                const retrying = isDelayed(e);
                const lastError = rcpts.find((r) => r.last_error)?.last_error;
                return (
                  <div
                    key={e.id}
                    role="listitem"
                    className="conv-row outbox-row"
                    onClick={() => e.sent_copy_id > 0 && navigate(`/mail/sent/message/${e.sent_copy_id}`)}
                  >
                    <span className="row-from">To: {pending.map((r) => r.rcpt).join(", ")}</span>
                    <span className="row-main">
                      <span className="row-subject">{e.subject || "(no subject)"}</span>
                      {retrying && lastError && <span className="row-snippet"> – Delivery delayed: {lastError}</span>}
                    </span>
                    <span className="row-end">
                      <span className={"row-date" + (retrying ? " warn" : "")} title={fullDate(next)}>
                        {retrying ? "Next try " : "Sends "}
                        {fullDate(next)}
                      </span>
                      {!retrying && (
                        <button
                          className="btn small"
                          onClick={(ev) => {
                            ev.stopPropagation();
                            void cancel(e);
                          }}
                        >
                          Cancel send
                        </button>
                      )}
                    </span>
                  </div>
                );
              })}
            </div>
          )}
        </div>
      </section>
    </div>
  );
}
