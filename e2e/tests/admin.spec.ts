import { expect, test } from "@playwright/test";
import { admin, apiSession, signIn, snap } from "../helpers";

const erin = { email: "erin@example.test", password: "erin-passw0rd!", name: "Erin Hall" };

test("shows the server overview", async ({ page }, testInfo) => {
  await signIn(page, admin);
  await page.getByRole("button", { name: `Account: ${admin.email}` }).click();
  await page.getByRole("menuitem", { name: "Administration" }).click();
  await expect(page).toHaveURL(/\/admin$/);
  await expect(page.getByRole("heading", { name: "Administration" })).toBeVisible();
  await expect(page.getByRole("heading", { name: "Ports" })).toBeVisible();
  await snap(page, testInfo, "16-admin-overview");
});

test("creates a user who can then sign in", async ({ page }, testInfo) => {
  await signIn(page, admin);
  await page.goto("/admin/users");
  await page.getByRole("button", { name: "Create user" }).click();
  const dialog = page.getByRole("dialog", { name: "Create user" });
  await dialog.getByLabel("User name").fill("erin");
  await dialog.getByLabel("Name", { exact: true }).fill(erin.name);
  await dialog.getByLabel("Password").fill(erin.password);
  await dialog.getByRole("button", { name: "Create" }).click();
  await expect(dialog).toBeHidden();
  const userRow = page.getByRole("row").filter({ hasText: erin.email });
  await expect(userRow).toContainText(erin.name);
  await snap(page, testInfo, "17-admin-users");

  const { api } = await apiSession(erin);
  await api.dispose();

  // Erin isn't an administrator.
  await page.context().clearCookies();
  await signIn(page, erin);
  await page.goto("/admin");
  await expect(page.getByText("Administrators only")).toBeVisible();
});

test("lists the DNS records a domain needs", async ({ page }, testInfo) => {
  await signIn(page, admin);
  await page.goto("/admin/domains");
  await page.getByRole("row").filter({ hasText: "example.test" }).getByRole("button", { name: "DNS records" }).click();
  const dialog = page.getByRole("dialog", { name: "DNS records for example.test" });
  await expect(dialog).toContainText("MX");
  await expect(dialog).toContainText("v=spf1");
  await expect(dialog).toContainText("v=DKIM1");
  await expect(dialog).toContainText("v=DMARC1");
  // Long record values must not squeeze the names into a few characters per line.
  const dkimName = dialog.locator(".dns-name").filter({ hasText: "._domainkey." });
  expect((await dkimName.boundingBox())!.height).toBeLessThan(30);
  await snap(page, testInfo, "18-admin-dns");
});

test("keeps undeliverable mail in the queue", async ({ page }, testInfo) => {
  await signIn(page, admin);
  await page.getByRole("button", { name: "Compose" }).click();
  const win = page.locator("section.compose-win");
  const to = win.getByLabel("To", { exact: true });
  await to.fill("someone@remote.test");
  await to.press(",");
  await expect(win.locator(".chip").filter({ hasText: "someone@remote.test" })).toBeVisible();
  await win.getByLabel("Subject", { exact: true }).fill("Hello from TitoMail");
  await win.locator(".editor").click();
  await page.keyboard.type("This one has to leave the server.");
  await win.getByRole("button", { name: "Send", exact: true }).click();
  await expect(page.locator(".toast").filter({ hasText: "Message sent." })).toBeVisible();

  // The test server never delivers remote mail, so the message waits in the queue.
  await page.goto("/admin/queue");
  const entry = page.getByRole("row").filter({ hasText: "Hello from TitoMail" });
  await expect(async () => {
    await page.getByRole("button", { name: "Refresh" }).click();
    await expect(entry).toContainText("outbound delivery is disabled", { timeout: 1_000 });
  }).toPass({ timeout: 30_000 });
  await expect(entry).toContainText("someone@remote.test");
  await snap(page, testInfo, "19-admin-queue");

  await entry.getByRole("button", { name: "Delete from queue" }).click();
  await page.getByRole("dialog", { name: "Delete this message from the queue?" }).getByRole("button", { name: "Delete" }).click();
  await expect(entry).toHaveCount(0);
  await expect(page.getByText("The queue is empty")).toBeVisible();
});
