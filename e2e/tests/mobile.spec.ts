import { expect, test } from "@playwright/test";
import { createUser, seedMailbox, signIn, snap, type Account } from "../helpers";

test.use({ viewport: { width: 390, height: 844 }, deviceScaleFactor: 2, isMobile: true, hasTouch: true });

let frank: Account;

test.beforeAll(async () => {
  frank = await createUser("frank", "Frank Ocean");
  await seedMailbox(frank);
});

test("works on a phone", async ({ page }, testInfo) => {
  await signIn(page, frank);
  const rows = page.locator(".conv-row");
  await expect(rows).toHaveCount(6);
  await expect(page.getByRole("navigation", { name: "Mailboxes" })).toBeHidden();
  await snap(page, testInfo, "20-mobile-inbox");

  await rows.filter({ hasText: "Photos from the offsite" }).locator(".row-subject").tap();
  await expect(page.getByRole("heading", { name: "Photos from the offsite" })).toBeVisible();
  await expect(rows).toHaveCount(0);
  await expect(page.locator(".att-card img")).toBeVisible();
  // The message header fits the screen with the sender's name on one line.
  const head = page.locator(".msg-head").first();
  expect(await head.evaluate((el) => el.scrollWidth <= el.clientWidth)).toBe(true);
  expect((await head.locator(".msg-from-name").boundingBox())!.height).toBeLessThan(30);
  await snap(page, testInfo, "21-mobile-conversation");
  await page.getByRole("button", { name: "Back to list" }).tap();
  await expect(rows).toHaveCount(6);

  await page.getByRole("button", { name: "Main menu" }).tap();
  const menu = page.getByRole("navigation", { name: "Mailboxes" });
  await expect(menu).toBeVisible();
  await snap(page, testInfo, "22-mobile-menu");
  await menu.getByRole("link", { name: "Sent" }).tap();
  await expect(page).toHaveURL(/\/mail\/sent$/);
  await expect(menu).toBeHidden();

  await page.getByRole("button", { name: "Compose" }).tap();
  const win = page.locator("section.compose-win");
  await expect(win).toBeVisible();
  const box = await win.boundingBox();
  expect(box?.width).toBe(390);
  await snap(page, testInfo, "23-mobile-compose");
});
