import { useQuery } from "@tanstack/react-query";
import { api } from "./api";
import type { Label, Mailbox } from "./types";

export const keys = {
  session: ["session"] as const,
  mailboxes: ["mailboxes"] as const,
  labels: ["labels"] as const,
  conversations: ["conversations"] as const,
  thread: ["thread"] as const,
  message: ["message"] as const,
  outbox: ["outbox"] as const,
};

const emptyMailboxes: Mailbox[] = [];
const emptyLabels: Label[] = [];

export function useMailboxes() {
  const q = useQuery({ queryKey: keys.mailboxes, queryFn: api.mailboxes, staleTime: 30_000 });
  return { ...q, mailboxes: q.data ?? emptyMailboxes };
}

export function useLabels() {
  const q = useQuery({ queryKey: keys.labels, queryFn: api.labels, staleTime: 30_000 });
  return { ...q, labels: q.data ?? emptyLabels };
}
