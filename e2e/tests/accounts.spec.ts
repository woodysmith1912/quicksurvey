import { expect, test } from '@playwright/test';
import { createSurvey, publish, signIn, userRow } from '../helpers';

// These tests change instance-wide state, so each one cleans up the accounts it
// creates. The suite runs with a single worker, in file order.
test.describe('accounts and roles', () => {
  const pw = 'account-test-pw';

  test('an admin can add a viewer and an editor', async ({ page }) => {
    await signIn(page);
    await page.goto('/admin/users');

    for (const [name, role] of [['val', 'viewer'], ['eve', 'editor']] as const) {
      await page.getByTestId('new-username').fill(name);
      await page.getByTestId('new-password').fill(pw);
      await page.getByTestId('new-role').selectOption(role);
      await page.getByTestId('add-user').click();
      await expect(page.getByTestId('flash')).toContainText('created');
      await expect(userRow(page, name)).toBeVisible();
    }
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

    await userRow(page, 'admin').getByTestId('delete-user').click();
    await expect(page.getByTestId('flash')).toContainText('cannot delete your own account');

    for (const name of ['val', 'eve']) {
      await userRow(page, name).getByTestId('delete-user').click();
      await expect(page.getByTestId('flash')).toContainText('deleted');
    }
    await expect(page.getByTestId('user-row')).toHaveCount(1);
  });
});
