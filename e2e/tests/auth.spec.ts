import { expect, test } from "@playwright/test";
import { admin, snap } from "../helpers";

test("asks to sign in and rejects a wrong password", async ({ page }) => {
  await page.goto("/mail/inbox");
  await expect(page.getByRole("heading", { name: "Sign in" })).toBeVisible();
  await page.locator("input[type=email]").fill(admin.email);
  await page.locator("input[type=password]").fill("not-the-password");
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByRole("alert")).toContainText(/invalid email or password/i);
  await expect(page.getByRole("button", { name: "Compose" })).toHaveCount(0);
});

test("signs in and out", async ({ page }, testInfo) => {
  await page.goto("/login");
  await page.locator("input[type=email]").fill(admin.email);
  await page.locator("input[type=password]").fill(admin.password);
  await snap(page, testInfo, "01-sign-in");
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page).toHaveURL(/\/mail\/inbox$/);
  await expect(page.getByRole("button", { name: "Compose" })).toBeVisible();

  await page.reload();
  await expect(page.getByRole("button", { name: "Compose" })).toBeVisible();

  await page.getByRole("button", { name: `Account: ${admin.email}` }).click();
  await page.getByRole("menuitem", { name: "Sign out" }).click();
  await expect(page.getByRole("heading", { name: "Sign in" })).toBeVisible();
  // The session is gone on the server too.
  await page.reload();
  await expect(page.getByRole("heading", { name: "Sign in" })).toBeVisible();
});

test("returns to the requested page after signing in", async ({ page }) => {
  await page.goto("/settings/account");
  await page.locator("input[type=email]").fill(admin.email);
  await page.locator("input[type=password]").fill(admin.password);
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByRole("link", { name: "Account", exact: true })).toHaveAttribute("aria-current", "page");
  await expect(page).toHaveURL(/\/settings\/account$/);
});
