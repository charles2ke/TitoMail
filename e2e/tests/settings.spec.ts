import { expect, test } from "@playwright/test";
import { createUser, seedMailbox, signIn, snap, type Account } from "../helpers";

let dave: Account;

test.beforeAll(async () => {
  dave = await createUser("dave", "Dave Kim");
  await seedMailbox(dave);
});

test("lists and handles keyboard shortcuts", async ({ page }, testInfo) => {
  await signIn(page, dave);
  await page.keyboard.press("?");
  const dialog = page.getByRole("dialog", { name: "Keyboard shortcuts" });
  await expect(dialog).toBeVisible();
  await snap(page, testInfo, "12-keyboard-shortcuts");
  await page.keyboard.press("Escape");
  await expect(dialog).toBeHidden();

  await page.keyboard.press("c");
  const win = page.locator("section.compose-win");
  await expect(win).toBeVisible();
  await win.getByRole("button", { name: "Discard draft" }).click();
  await expect(win).toBeHidden();
  await page.keyboard.press("g");
  await page.keyboard.press("s");
  await expect(page).toHaveURL(/\/mail\/starred$/);
});

test("shows how to set up mail apps", async ({ page }, testInfo) => {
  await signIn(page, dave);
  await page.goto("/settings/apps");
  await expect(page.getByText("mail.example.test").first()).toBeVisible();
  await expect(page.getByText(dave.email).first()).toBeVisible();
  await snap(page, testInfo, "13-mail-apps");
});

test("switches to a dark, compact layout that persists", async ({ page }, testInfo) => {
  await signIn(page, dave);
  await page.getByRole("link", { name: "Settings" }).click();
  await expect(page).toHaveURL(/\/settings$/);
  await page.getByRole("radio", { name: "Dark" }).check();
  await page.getByRole("radio", { name: "Compact" }).check();
  await page.getByRole("button", { name: "Save changes" }).click();
  await expect(page.locator(".toast").filter({ hasText: "Settings saved." })).toBeVisible();
  const root = page.locator("html");
  await expect(root).toHaveAttribute("data-theme", "dark");
  await expect(root).toHaveAttribute("data-density", "compact");
  await snap(page, testInfo, "14-settings-dark");

  // The choice is stored on the server, not just in this tab.
  await page.goto("/mail/inbox");
  await expect(root).toHaveAttribute("data-theme", "dark");
  await page.locator(".conv-row").filter({ hasText: "Welcome to TitoMail" }).locator(".row-subject").click();
  await expect(page.getByRole("heading", { name: "Welcome to TitoMail" })).toBeVisible();
  await snap(page, testInfo, "15-dark-mode");
});
