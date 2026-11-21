import { expect, test, type Page } from "@playwright/test";
import fs from "node:fs";
import { createUser, seedMailbox, sendMail, signIn, snap, type Account } from "../helpers";

// These tests tell one story in a shared mailbox, so they run in order.
test.describe.configure({ mode: "serial" });

let bob: Account;
let carol: Account;

const rows = (page: Page) => page.locator(".conv-row");
const row = (page: Page, subject: string) => rows(page).filter({ hasText: subject });
const sidebar = (page: Page) => page.getByRole("navigation", { name: "Mailboxes" });
const toast = (page: Page, text: string) => page.locator(".toast").filter({ hasText: text });
const lastMessageFrame = (page: Page) => page.locator('iframe[title="Message"]').last().contentFrame();

test.beforeAll(async () => {
  bob = await createUser("bob", "Bob Martin");
  carol = await createUser("carol", "Carol Shaw");
  await seedMailbox(bob);
});

test("lists the inbox as conversations", async ({ page }, testInfo) => {
  await signIn(page, bob);
  // Seven messages, two of which are one conversation.
  await expect(rows(page)).toHaveCount(6);
  await expect(row(page, "Lunch on Friday?").locator(".row-count")).toHaveText("2");
  await expect(row(page, "Welcome to TitoMail")).toHaveClass(/unread/);
  await expect(row(page, "Photos from the offsite").getByLabel("Has attachments")).toBeVisible();
  await snap(page, testInfo, "02-inbox");
});

test("shows new mail as soon as it arrives", async ({ page }) => {
  await signIn(page, bob);
  await expect(rows(page)).toHaveCount(6);
  await sendMail({
    from: { name: "Ops Team", address: "ops@infra.test" },
    to: [{ name: bob.name, address: bob.email }],
    subject: "Planned maintenance tonight",
    text: "The VPN will be unavailable from 22:00 to 23:00 UTC.",
  });
  // No reload: the list updates from the server's event stream.
  await expect(row(page, "Planned maintenance tonight")).toBeVisible();
  await expect(rows(page)).toHaveCount(7);
  await expect(rows(page).first()).toContainText("Planned maintenance tonight");
});

test("reads a message with an image attachment", async ({ page }, testInfo) => {
  await signIn(page, bob);
  await row(page, "Photos from the offsite").locator(".row-subject").click();
  await expect(page).toHaveURL(/\/mail\/inbox\/thread\/\d+$/);
  await expect(page.getByRole("heading", { name: "Photos from the offsite" })).toBeVisible();
  await expect(lastMessageFrame(page).getByText("worth the climb")).toBeVisible();
  const card = page.locator(".att-card").filter({ hasText: "offsite.png" });
  await expect(card).toBeVisible();
  await expect.poll(() => card.locator("img").evaluate((img: HTMLImageElement) => img.naturalWidth)).toBe(480);
  await expect(row(page, "Photos from the offsite")).not.toHaveClass(/unread/);
  await snap(page, testInfo, "03-message-with-attachment");
});

test("downloads an attachment", async ({ page }) => {
  await signIn(page, bob);
  await row(page, "Your invoice #4821").locator(".row-subject").click();
  await expect(lastMessageFrame(page).getByText("will be charged on November 15")).toBeVisible();
  const [download] = await Promise.all([page.waitForEvent("download"), page.getByLabel("Download invoice-4821.pdf").click()]);
  expect(download.suggestedFilename()).toBe("invoice-4821.pdf");
  const body = fs.readFileSync(await download.path());
  expect(body.subarray(0, 8).toString()).toBe("%PDF-1.4");
});

test("shows a conversation with quoted text folded away", async ({ page }, testInfo) => {
  await signIn(page, bob);
  await row(page, "Lunch on Friday?").locator(".row-subject").click();
  await expect(page.locator(".thread-messages .msg")).toHaveCount(2);
  const reply = lastMessageFrame(page);
  await expect(reply.getByText("Count me in!")).toBeVisible();
  // The quoted original is hidden until asked for.
  await expect(reply.getByText("Shall we try the new noodle place")).toBeHidden();
  await page.getByRole("button", { name: "Show trimmed content" }).click();
  await expect(reply.getByText("Shall we try the new noodle place")).toBeVisible();
  await snap(page, testInfo, "04-conversation");
});

test("writes a formatted message with address autocomplete", async ({ page }, testInfo) => {
  await signIn(page, bob);
  await page.getByRole("button", { name: "Compose" }).click();
  const win = page.locator("section.compose-win");
  const to = win.getByLabel("To", { exact: true });
  await expect(to).toBeFocused();
  await to.pressSequentially("car");
  await page.getByRole("option", { name: /Carol Shaw/ }).click();
  await expect(win.locator(".chip").filter({ hasText: "Carol Shaw" })).toBeVisible();

  await win.getByLabel("Subject", { exact: true }).fill("Project kickoff");
  await win.locator(".editor").click();
  await page.keyboard.type("Hi Carol,");
  await page.keyboard.press("Enter");
  await page.keyboard.press("Enter");
  await page.keyboard.type("Let's meet on ");
  await page.keyboard.press("ControlOrMeta+b");
  await page.keyboard.type("Friday at 10:00");
  await page.keyboard.press("ControlOrMeta+b");
  await page.keyboard.type(" to plan the launch.");
  await expect(win).toHaveAccessibleName("Project kickoff");
  await snap(page, testInfo, "05-compose");

  await win.getByRole("button", { name: "Send", exact: true }).click();
  const sent = toast(page, "Message sent.");
  await expect(sent).toBeVisible();
  await expect(sent.getByRole("button", { name: "Undo" })).toBeVisible();
  await expect(win).toBeHidden();

  await sidebar(page).getByRole("link", { name: "Sent" }).click();
  await expect(row(page, "Project kickoff")).toBeVisible();
  await expect(row(page, "Project kickoff")).toContainText("To: Carol Shaw");
});

test("the recipient replies and both see one conversation", async ({ page, browser }, testInfo) => {
  const carolContext = await browser.newContext();
  const carolPage = await carolContext.newPage();
  await signIn(carolPage, carol);
  // Delivery waits for the sender's undo window to pass.
  const incoming = row(carolPage, "Project kickoff");
  await expect(incoming).toBeVisible({ timeout: 30_000 });
  await expect(incoming).toContainText("Bob Martin");
  // The snippet keeps the space after the bold text.
  await expect(incoming).toContainText("Friday at 10:00 to plan the launch");
  await incoming.locator(".row-subject").click();
  const original = lastMessageFrame(carolPage);
  await expect(original.getByText("to plan the launch")).toBeVisible();
  await expect(original.locator("b, strong").filter({ hasText: "Friday at 10:00" })).toBeVisible();

  await carolPage.locator(".thread-replies").getByRole("button", { name: "Reply" }).click();
  const reply = carolPage.locator("section.compose-win");
  await expect(reply.getByLabel("Subject", { exact: true })).toHaveValue("Re: Project kickoff");
  await expect(reply.locator(".chip").filter({ hasText: "Bob Martin" })).toBeVisible();
  await expect(reply.locator(".editor")).toBeFocused();
  await carolPage.keyboard.type("Sounds great, see you on Friday!");
  await snap(carolPage, testInfo, "06-reply");
  await reply.getByRole("button", { name: "Send", exact: true }).click();
  await expect(toast(carolPage, "Message sent.")).toBeVisible();
  await carolContext.close();

  await signIn(page, bob);
  const thread = row(page, "Project kickoff");
  await expect(thread).toBeVisible({ timeout: 30_000 });
  await expect(thread.locator(".row-count")).toHaveText("2");
  await thread.locator(".row-subject").click();
  await expect(page.locator(".thread-messages .msg")).toHaveCount(2);
  await expect(lastMessageFrame(page).getByText("Sounds great, see you on Friday!")).toBeVisible();
  await snap(page, testInfo, "07-threaded-reply");
});

test("searches mail", async ({ page }, testInfo) => {
  await signIn(page, bob);
  const box = page.getByRole("textbox", { name: "Search mail" });
  await box.fill("invoice");
  await box.press("Enter");
  await expect(page).toHaveURL(/\/mail\/search\?q=invoice$/);
  await expect(rows(page)).toHaveCount(1);
  await expect(row(page, "Your invoice #4821 for October")).toBeVisible();
  await snap(page, testInfo, "08-search");

  await box.fill("has:attachment");
  await box.press("Enter");
  await expect(rows(page)).toHaveCount(2);
  await expect(row(page, "Photos from the offsite")).toBeVisible();

  await box.fill("from:carol");
  await box.press("Enter");
  await expect(rows(page)).toHaveCount(1);
  await expect(row(page, "Project kickoff")).toBeVisible();
});

test("organizes mail with labels", async ({ page }, testInfo) => {
  await signIn(page, bob);
  await sidebar(page).getByRole("button", { name: "Create label" }).click();
  const dialog = page.getByRole("dialog", { name: "New label" });
  await dialog.getByLabel("Label name").fill("Receipts");
  await dialog.getByRole("radio").nth(3).click();
  await dialog.getByRole("button", { name: "Create" }).click();
  await expect(dialog).toBeHidden();
  const labelLink = sidebar(page).getByRole("link", { name: "Receipts" });
  await expect(labelLink).toBeVisible();

  const invoice = row(page, "Your invoice #4821");
  await invoice.getByRole("checkbox").check();
  await expect(page.locator(".selection-count")).toHaveText("1 selected");
  await page.locator(".list-toolbar").getByRole("button", { name: "Labels" }).click();
  await page.getByRole("checkbox", { name: "Receipts" }).check();
  await expect(toast(page, "added to “Receipts”")).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(invoice.locator(".chip").filter({ hasText: "Receipts" })).toBeVisible();

  await labelLink.click();
  await expect(page).toHaveURL(/\/mail\/label\/\d+$/);
  await expect(rows(page)).toHaveCount(1);
  await expect(row(page, "Your invoice #4821")).toBeVisible();
  await snap(page, testInfo, "09-label");
});

test("stars, archives and undoes", async ({ page }, testInfo) => {
  await signIn(page, bob);
  const digest = row(page, "This week: Go 1.27");
  await digest.getByRole("button", { name: "Not starred" }).click();
  await expect(digest.getByRole("button", { name: "Starred" })).toBeVisible();
  await sidebar(page).getByRole("link", { name: "Starred" }).click();
  await expect(page).toHaveURL(/\/mail\/starred$/);
  await expect(rows(page)).toHaveCount(1);
  await expect(row(page, "This week: Go 1.27")).toBeVisible();

  await sidebar(page).getByRole("link", { name: "Inbox" }).click();
  await row(page, "Lunch on Friday?").locator(".row-subject").click();
  await page.locator(".thread-toolbar").getByRole("button", { name: "Archive" }).click();
  const archived = toast(page, "Conversation archived.");
  await expect(archived).toBeVisible();
  await expect(row(page, "Lunch on Friday?")).toHaveCount(0);
  await snap(page, testInfo, "10-archived");
  await archived.getByRole("button", { name: "Undo" }).click();
  await expect(toast(page, "Action undone.")).toBeVisible();
  await expect(row(page, "Lunch on Friday?")).toBeVisible();
});

test("remembers people you've written to", async ({ page }, testInfo) => {
  await signIn(page, bob);
  await page.getByRole("button", { name: `Account: ${bob.email}` }).click();
  await page.getByRole("menuitem", { name: "Contacts" }).click();
  await expect(page).toHaveURL(/\/contacts$/);
  await page.getByRole("button", { name: /Other contacts/ }).click();
  const carolRow = page.getByRole("row").filter({ hasText: carol.email });
  await expect(carolRow).toContainText("Carol Shaw");
  await snap(page, testInfo, "11-contacts");
});
