import { expect, test, type Page } from '@playwright/test';
import { deleteAccount, signIn, userRow } from '../helpers';

/** Creates an invitation as an admin and returns the one-time link. */
async function createInvite(page: Page, role: string, note = 'come help out') {
  await page.goto('/admin/users');
  await page.getByTestId('invite-role').selectOption(role);
  await page.getByTestId('invite-note').fill(note);
  await page.getByTestId('create-invite').click();
  const url = (await page.getByTestId('invite-url').innerText()).trim();
  expect(url).toContain('/invite/');
  return url;
}

test.describe('invitation links', () => {
  test('invite, self-register, approve — and the role is the approver\'s choice', async ({ page, browser }) => {
    await signIn(page);
    const link = await createInvite(page, 'admin', 'you should be an admin');

    // Someone with no account opens the link.
    const ctx = await browser.newContext();
    const guest = await ctx.newPage();
    await guest.goto(link);
    await expect(guest.getByTestId('invite-inviter')).toHaveText('admin');
    await expect(guest.getByTestId('invite-suggested-role')).toHaveText('admin');
    await expect(guest.getByTestId('invite-note-shown')).toHaveText('you should be an admin');

    await guest.getByTestId('invite-username').fill('newbie');
    await guest.getByTestId('invite-password').fill('password123');
    await guest.getByTestId('invite-confirm').fill('password123');
    await guest.getByTestId('invite-submit').click();

    // Signed in, but parked until approved.
    await expect(guest).toHaveURL(/\/account\/pending/);
    await expect(guest.getByTestId('pending-notice')).toContainText('newbie');
    await guest.goto('/admin/');
    await expect(guest).toHaveURL(/\/account\/pending/);

    // The admin downgrades them to viewer on approval.
    await page.goto('/admin/users');
    const row = page.locator('[data-testid="pending-user"][data-user="newbie"]');
    await expect(row).toBeVisible();
    await row.getByTestId('approve-role').selectOption('viewer');
    await row.getByTestId('approve-user').click();
    await expect(page.getByTestId('flash')).toContainText('approved as viewer');
    await expect(page.locator('[data-testid="pending-user"]')).toHaveCount(0);
    await expect(userRow(page, 'newbie')).toBeVisible();

    // The account now works, with the approver's role and not the invite's.
    await guest.goto('/admin/');
    await expect(guest.getByTestId('whoami')).toContainText('viewer');
    await guest.goto('/admin/users');
    await expect(guest.getByTestId('error')).toContainText('permission');

    // Cleanup so later tests see a clean account list.
    await deleteAccount(page, 'newbie');
    await ctx.close();
  });

  test('the link is one-time: a second person cannot reuse it', async ({ page, browser }) => {
    await signIn(page);
    const link = await createInvite(page, 'editor');

    const firstCtx = await browser.newContext();
    const first = await firstCtx.newPage();
    await first.goto(link);
    await first.getByTestId('invite-username').fill('early');
    await first.getByTestId('invite-password').fill('password123');
    await first.getByTestId('invite-confirm').fill('password123');
    await first.getByTestId('invite-submit').click();
    await expect(first).toHaveURL(/\/account\/pending/);

    // The same URL is now dead for everyone else.
    const secondCtx = await browser.newContext();
    const second = await secondCtx.newPage();
    await second.goto(link);
    await expect(second.getByTestId('error')).toBeVisible();
    await expect(second.getByTestId('invite-username')).toHaveCount(0);

    // Reject the outstanding request, and the account goes with it.
    await page.goto('/admin/users');
    await page.locator('[data-testid="pending-user"][data-user="early"]').getByTestId('reject-user').click();
    await expect(page.getByTestId('flash')).toContainText('rejected');
    await expect(page.locator('[data-testid="pending-user"]')).toHaveCount(0);

    await firstCtx.close();
    await secondCtx.close();
  });

  test('an admin can revoke an unused link', async ({ page, browser }) => {
    await signIn(page);
    const link = await createInvite(page, 'viewer');

    await page.goto('/admin/users');
    const row = page.locator('[data-testid="invite-row"]').first();
    await expect(row.getByTestId('invite-status')).toHaveText('open');
    await row.getByTestId('revoke-invite').click();
    await expect(page.getByTestId('flash')).toContainText('revoked');
    await expect(page.locator('[data-testid="invite-row"]').first().getByTestId('invite-status'))
      .toHaveText('revoked');

    const ctx = await browser.newContext();
    const guest = await ctx.newPage();
    await guest.goto(link);
    await expect(guest.getByTestId('error')).toBeVisible();
    await ctx.close();
  });

  test('a non-admin is offered no way to invite anyone', async ({ page, browser }) => {
    await signIn(page);
    // Make an editor the direct way.
    await page.goto('/admin/users');
    await page.getByTestId('new-username').fill('ed');
    await page.getByTestId('new-password').fill('password123');
    await page.getByTestId('new-role').selectOption('editor');
    await page.getByTestId('add-user').click();

    const ctx = await browser.newContext();
    const editor = await ctx.newPage();
    await signIn(editor, 'ed', 'password123');
    await editor.goto('/admin/users');
    await expect(editor.getByTestId('error')).toContainText('permission');
    await expect(editor.getByTestId('create-invite')).toHaveCount(0);
    await ctx.close();

    await page.goto('/admin/users');
    await deleteAccount(page, 'ed');
  });
});
