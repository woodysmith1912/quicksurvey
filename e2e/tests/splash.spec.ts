import { expect, test } from '@playwright/test';
import { signIn } from '../helpers';

test.describe('front page', () => {
  test('explains the site and keeps sign-in out of the way', async ({ page }) => {
    await page.goto('/');
    await expect(page.getByRole('heading', { level: 1 })).toContainText('what people actually want');
    await expect(page.getByText('which of these are you interested in')).toBeVisible();

    // Four value propositions, not a login form.
    await expect(page.locator('.point')).toHaveCount(4);
    await expect(page.locator('input[name=password]')).toHaveCount(0);

    // The sign-in link is in the header, to the right of the brand.
    const link = page.getByTestId('signin-link');
    await expect(link).toBeVisible();
    const brandBox = (await page.locator('.brand').boundingBox())!;
    const linkBox = (await link.boundingBox())!;
    expect(linkBox.x).toBeGreaterThan(brandBox.x);
    await link.click();
    await expect(page).toHaveURL(/\/login/);
  });

  test('a signed-in visitor is taken to their surveys instead', async ({ page }) => {
    await signIn(page);
    await page.goto('/');
    await expect(page).toHaveURL(/\/admin\//);
  });
});
