import { expect, type Browser, type BrowserContext, type Page } from '@playwright/test';
import { ADMIN } from './env';

/**
 * Creates an account the only way the web offers: an invitation the admin
 * mints and the invitee claims, choosing their own password. There is no form
 * for an admin to set someone's first password, deliberately.
 */
export async function inviteAndClaim(
  page: Page, browser: Browser, name: string, password: string, role: string,
) {
  await page.goto('/admin/users');
  await page.getByTestId('invite-role').selectOption(role);
  await page.getByTestId('create-invite').click();
  const link = (await page.getByTestId('invite-url').innerText()).trim();

  const ctx = await browser.newContext();
  const guest = await ctx.newPage();
  await guest.goto(link);
  await guest.getByTestId('invite-username').fill(name);
  await guest.getByTestId('invite-password').fill(password);
  await guest.getByTestId('invite-confirm').fill(password);
  await guest.getByTestId('invite-submit').click();
  await ctx.close();

  // Approve them, with the role the invitation suggested.
  await page.goto('/admin/users');
  const row = page.locator(`[data-testid="pending-user"][data-user="${name}"]`);
  await row.getByTestId('approve-role').selectOption(role);
  await row.getByTestId('approve-user').click();
  await expect(page.getByTestId('flash')).toContainText('approved');
}

/** Signs in on the given page. Defaults to the seeded administrator. */
export async function signIn(page: Page, user = ADMIN.user, password = ADMIN.password) {
  await page.goto('/login');
  await page.getByLabel('Username').fill(user);
  await page.getByLabel('Password').fill(password);
  await page.getByTestId('signin').click();
  await expect(page.getByTestId('whoami')).toContainText(user);
}

/**
 * Creates a survey through the UI and returns its ID and respondent URL.
 * It is left as a draft; call publish() when the test needs it live.
 */
export async function createSurvey(page: Page, title: string, options: string[]) {
  await page.goto('/admin/');
  // The form is inside a <details> that collapses once the list is non-empty.
  const form = page.locator('details.card').first();
  if (!(await form.evaluate((d) => (d as HTMLDetailsElement).open))) {
    await page.getByTestId('new-survey-toggle').click();
  }
  await page.getByTestId('new-title').fill(title);
  await page.getByTestId('new-options').fill(options.join('\n'));
  await page.getByTestId('create-survey').click();

  // Creation lands on the edit page, whose URL carries the ID.
  await expect(page).toHaveURL(/\/admin\/s\/[a-z0-9]+\/edit$/);
  const id = page.url().match(/\/admin\/s\/([a-z0-9]+)\/edit$/)![1];
  return { id, adminUrl: `/admin/s/${id}`, url: `/s/${id}` };
}

/** Signs in as a fresh, isolated browser context. */
export async function asUser(browser: Browser, user: string, password: string) {
  const ctx = await browser.newContext();
  const page = await ctx.newPage();
  await signIn(page, user, password);
  return { ctx, page };
}

/** Moves a draft survey to open, from its admin page. */
export async function publish(page: Page, adminUrl: string) {
  await page.goto(adminUrl);
  await page.getByTestId('open-survey').click();
  await expect(page.getByTestId('state')).toHaveText('open');
}

/**
 * A fresh browser context, which is what makes a "different person": the voter
 * cookie is what deduplicates responses, and contexts do not share cookies.
 */
export async function respondent(browser: Browser): Promise<BrowserContext> {
  return browser.newContext();
}

/** Clicks the thumbs-up for each named option, matching the label exactly. */
export async function thumbsUp(page: Page, ...labels: string[]) {
  for (const label of labels) {
    await page.getByTestId('option').filter({ hasText: new RegExp(`^${escapeRe(label)}`) }).first().click();
  }
}

/**
 * Addresses one row of the accounts table by username. Filtering rows by text
 * would be ambiguous: every row's role <select> contains the word "admin".
 */
export function userRow(page: Page, name: string) {
  return page.locator(`[data-testid="user-row"][data-user="${name}"]`);
}

/**
 * Deletes an account. Deletion cannot be undone, so the UI asks for the
 * username to be retyped — the same guard survey deletion uses.
 */
export async function deleteAccount(page: Page, name: string) {
  const row = userRow(page, name);
  await row.getByTestId('delete-user-toggle').click();
  await row.getByTestId('delete-user-confirm').fill(name);
  await row.getByTestId('delete-user').click();
  await expect(page.getByTestId('flash')).toContainText('deleted');
}

/** Reads the tally as a plain object of option text to vote count. */
export async function tally(page: Page): Promise<Record<string, number>> {
  const out: Record<string, number> = {};
  for (const row of await page.getByTestId('tally-row').all()) {
    out[(await row.getAttribute('data-option'))!] = Number(await row.getByTestId('votes').innerText());
  }
  return out;
}

/** Reads the tally's "Shown to" column as option text to respondent count. */
export async function shownTo(page: Page): Promise<Record<string, number>> {
  const out: Record<string, number> = {};
  for (const row of await page.getByTestId('tally-row').all()) {
    out[(await row.getAttribute('data-option'))!] = Number(await row.getByTestId('shown').innerText());
  }
  return out;
}

function escapeRe(s: string) {
  return s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}

/** Parses TSV into a header row plus records keyed by column name. */
export function parseTsv(text: string) {
  const [head, ...rest] = text.trimEnd().split('\n').map((l) => l.split('\t'));
  return {
    header: head,
    rows: rest.map((cells) => Object.fromEntries(cells.map((c, i) => [head[i], c]))),
  };
}
