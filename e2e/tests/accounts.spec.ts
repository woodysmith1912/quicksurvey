import { expect, test } from '@playwright/test';
import { createSurvey, deleteAccount, inviteAndClaim, publish, signIn, userRow } from '../helpers';

// These tests change instance-wide state, so each one cleans up the accounts it
// creates. The suite runs with a single worker, in file order.
test.describe('accounts and roles', () => {
  const pw = 'account-test-pw';

  test('an admin can add a viewer and an editor, by invitation only', async ({ page, browser }) => {
    await signIn(page);
    await page.goto('/admin/users');

    for (const [name, role] of [['val', 'viewer'], ['eve', 'editor']] as const) {
      await inviteAndClaim(page, browser, name, pw, role);
      await expect(userRow(page, name)).toBeVisible();
    }
    // And there is no way for an admin to choose someone's first password.
    await expect(page.getByTestId('add-user')).toHaveCount(0);
    await expect(page.getByTestId('new-password')).toHaveCount(0);
  });

  test('a viewer reads results and exports but cannot edit', async ({ page, browser }) => {
    await signIn(page);
    const s = await createSurvey(page, 'Role check', ['One']);
    await publish(page, s.adminUrl);

    const ctx = await browser.newContext();
    const viewer = await ctx.newPage();
    await signIn(viewer, 'val', pw);

    await viewer.goto(s.adminUrl);
    await expect(viewer.getByTestId('tally')).toBeVisible();
    await expect(viewer.getByTestId('export-responses')).toBeVisible();
    // No authoring controls are offered...
    await expect(viewer.getByTestId('edit-link')).toHaveCount(0);
    await expect(viewer.getByTestId('close-survey')).toHaveCount(0);
    await expect(viewer.getByTestId('delete-survey')).toHaveCount(0);
    // ...and the URL is refused even if typed directly.
    await viewer.goto(s.adminUrl + '/edit');
    await expect(viewer.getByTestId('error')).toContainText('permission');
    await ctx.close();
  });

  test('an editor manages surveys but not accounts', async ({ page, browser }) => {
    await signIn(page);
    const s = await createSurvey(page, 'Editor check', ['One']);

    const ctx = await browser.newContext();
    const editor = await ctx.newPage();
    await signIn(editor, 'eve', pw);

    await editor.goto(s.adminUrl + '/edit');
    await expect(editor.getByTestId('save-survey')).toBeVisible();
    // The Accounts link is not even offered to a non-admin.
    await expect(editor.getByRole('link', { name: 'Accounts' })).toHaveCount(0);
    await editor.goto('/admin/users');
    await expect(editor.getByTestId('error')).toContainText('permission');
    await ctx.close();
  });

  test('changing a password signs that account out everywhere else', async ({ page, browser }) => {
    const laptopCtx = await browser.newContext();
    const phoneCtx = await browser.newContext();
    const laptop = await laptopCtx.newPage();
    const phone = await phoneCtx.newPage();
    await signIn(laptop, 'val', pw);
    await signIn(phone, 'val', pw);

    const newPw = 'a-completely-new-one';
    await laptop.goto('/account/password');
    await laptop.getByLabel('Current password').fill(pw);
    await laptop.getByLabel('New password', { exact: true }).fill(newPw);
    await laptop.getByLabel('Repeat new password').fill(newPw);
    await laptop.getByTestId('save-password').click();
    await expect(laptop.getByTestId('flash')).toContainText('Password changed');

    // The other device is back at the sign-in page.
    await phone.goto('/admin/');
    await expect(phone).toHaveURL(/\/login/);

    // Restore the password so the later cleanup test reads naturally.
    await laptop.goto('/account/password');
    await laptop.getByLabel('Current password').fill(newPw);
    await laptop.getByLabel('New password', { exact: true }).fill(pw);
    await laptop.getByLabel('Repeat new password').fill(pw);
    await laptop.getByTestId('save-password').click();

    await laptopCtx.close();
    await phoneCtx.close();
  });

  test('an admin cannot delete their own account, and can delete others', async ({ page }) => {
    await signIn(page);
    await page.goto('/admin/users');

    const me = userRow(page, 'admin');
    await me.getByTestId('delete-user-toggle').click();
    await me.getByTestId('delete-user-confirm').fill('admin');
    await me.getByTestId('delete-user').click();
    await expect(page.getByTestId('flash')).toContainText('cannot delete your own account');

    for (const name of ['val', 'eve']) {
      await deleteAccount(page, name);
    }
    await expect(page.getByTestId('user-row')).toHaveCount(1);
  });

  test('an admin hands out a reset link and never sees the password', async ({ page, browser }) => {
    await signIn(page);
    await inviteAndClaim(page, browser, 'resetme', 'password123', 'viewer');

    // The admin can only generate a link — there is no field to type someone
    // else's password into.
    await userRow(page, 'resetme').getByTestId('reset-link').click();
    const link = (await page.getByTestId('reset-url').innerText()).trim();
    expect(link).toContain('/reset/');
    expect(page.url()).not.toContain('new_reset');

    // The account owner chooses it.
    const ctx = await browser.newContext();
    const owner = await ctx.newPage();
    await owner.goto(link);
    await expect(owner.getByTestId('reset-user')).toHaveText('resetme');
    await owner.getByTestId('reset-password').fill('chosen-by-the-owner');
    await owner.getByTestId('reset-confirm').fill('chosen-by-the-owner');
    await owner.getByTestId('reset-submit').click();
    await expect(owner).toHaveURL(/\/login/);

    await signIn(owner, 'resetme', 'chosen-by-the-owner');
    await expect(owner.getByTestId('whoami')).toContainText('resetme');

    // Used once, and only once.
    const second = await browser.newContext();
    const other = await second.newPage();
    await other.goto(link);
    await expect(other.getByTestId('error')).toBeVisible();

    await ctx.close();
    await second.close();
    await page.goto('/admin/users');
    await deleteAccount(page, 'resetme');
  });

});
