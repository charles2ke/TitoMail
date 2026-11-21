import { expect, test } from "@playwright/test";
import { createUser, signIn, snap } from "../helpers";

test("scheduled messages remain visible and cancellable until claimed", async ({ page }, testInfo) => {
  const account = await createUser("scheduled", "Scheduled Sender");
  await signIn(page, account);
  const session = await (await page.request.get("/api/session")).json();
  const send = async (subject: string, scheduleAt?: Date) => {
    const res = await page.request.post("/api/send", {
      headers: { "X-CSRF-Token": session.csrf_token },
      data: {
        from: account.email,
        to: [{ address: "one@remote.test" }, { address: "two@other.test" }],
        subject,
        html: "<p>Scheduled message</p>",
        ...(scheduleAt && { schedule_at: scheduleAt.toISOString() }),
      },
    });
    expect(res.status(), await res.text()).toBe(200);
    return res.json();
  };
  const soon = await send("Soon", new Date(Date.now() + 50_000));
  const laterAt = new Date(Date.now() + 2 * 60 * 60_000);
  const later = await send("Later", laterAt);
  const undo = await send("Undo only");
  expect(soon.scheduled).toBe(true);
  expect(later.scheduled).toBe(true);
  expect(undo.scheduled).toBe(false);

  await page.goto("/mail/scheduled");
  await expect(page.locator(".outbox-row").filter({ hasText: "Soon" })).toBeVisible();
  await expect(page.locator(".outbox-row").filter({ hasText: "Later" })).toBeVisible();
  await expect(page.locator(".outbox-row").filter({ hasText: "Undo only" })).toHaveCount(0);
  await page.reload();
  await expect(page.locator(".outbox-row").filter({ hasText: "Soon" })).toBeVisible();

  // Advance only the browser clock, so the server keeps both entries unclaimed.
  await page.clock.setFixedTime(new Date(laterAt.getTime() - 30_000));
  await page.reload();
  await expect(page.locator(".outbox-row").filter({ hasText: "Later" })).toBeVisible();
  await snap(page, testInfo, "scheduled-outbox");

  // A single claimed recipient makes the whole message uncancellable.
  await page.route("**/api/outbox", async (route) => {
    const response = await route.fetch();
    const entries = await response.json();
    const entry = entries.find((e: { id: number }) => e.id === later.queue_id);
    entry.recipients[0].last_attempt_at = new Date().toISOString();
    await route.fulfill({ response, json: entries });
  });
  await page.reload();
  await expect(page.locator(".outbox-row").filter({ hasText: "Later" })).toHaveCount(0);
  await page.unroute("**/api/outbox");
  await page.reload();
  await page.locator(".outbox-row").filter({ hasText: "Later" }).getByRole("button", { name: "Cancel send" }).click();
  await expect(page.getByText("Sending canceled. The message was moved to Drafts.")).toBeVisible();
  await expect(page.locator(".outbox-row").filter({ hasText: "Later" })).toHaveCount(0);
});
