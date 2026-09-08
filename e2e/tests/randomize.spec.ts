import { expect, test, type Page } from '@playwright/test';
import { createSurvey, publish, respondent, signIn, tally } from '../helpers';

/** Reads the option labels in the order the ballot presents them. */
async function order(page: Page): Promise<string[]> {
  return (await page.getByTestId('option').allInnerTexts()).map((t) => t.trim());
}

const OPTIONS = ['Alpha', 'Bravo', 'Charlie', 'Delta', 'Echo', 'Foxtrot', 'Golf', 'Hotel'];

test.describe('option order', () => {
  test('each respondent gets their own order, and it does not move under them', async ({ page, browser }) => {
    await signIn(page);
    const s = await createSurvey(page, 'Order matters', OPTIONS);
    await publish(page, s.adminUrl);
    await expect(page.getByTestId('randomized-note')).toBeVisible();

    const ctx = await respondent(browser);
    const voter = await ctx.newPage();
    await voter.goto(s.url);
    const mine = await order(voter);
    expect(mine).toHaveLength(OPTIONS.length);
    expect([...mine].sort()).toEqual([...OPTIONS].sort());   // same set, some order

    // Reloading must not reshuffle: the respondent's ticks would appear to move.
    await voter.reload();
    expect(await order(voter)).toEqual(mine);

    // Nor may voting reshuffle it.
    await voter.getByTestId('option').first().click();
    await voter.getByTestId('submit-vote').click();
    await voter.goto(s.url);
    expect(await order(voter)).toEqual(mine);
    await expect(voter.getByTestId('option').filter({ hasText: mine[0] }).locator('..')
      .locator('input')).toBeChecked();

    // Other people see other orders.
    const orders = new Set([mine.join('|')]);
    for (let i = 0; i < 5; i++) {
      const other = await respondent(browser);
      const p = await other.newPage();
      await p.goto(s.url);
      orders.add((await order(p)).join('|'));
      await other.close();
    }
    expect(orders.size).toBeGreaterThan(1);
    await ctx.close();
  });

  test('an editor can turn it off, and then everyone sees the written order', async ({ page, browser }) => {
    await signIn(page);
    const s = await createSurvey(page, 'Fixed order', OPTIONS);
    await publish(page, s.adminUrl);

    await page.goto(s.adminUrl + '/edit');
    await expect(page.getByTestId('randomize')).toBeChecked();   // on by default
    await page.getByTestId('randomize').uncheck();
    await page.getByTestId('save-survey').click();

    await page.goto(s.adminUrl);
    await expect(page.getByTestId('randomized-note')).toHaveCount(0);

    for (let i = 0; i < 3; i++) {
      const ctx = await respondent(browser);
      const p = await ctx.newPage();
      await p.goto(s.url);
      expect(await order(p)).toEqual(OPTIONS);
      await ctx.close();
    }
  });

  test('shuffling does not disturb the tally', async ({ page, browser }) => {
    await signIn(page);
    const s = await createSurvey(page, 'Counting is unaffected', ['Alpha', 'Bravo', 'Charlie']);
    await publish(page, s.adminUrl);

    // Three people all pick Bravo, whatever position it appeared in for them.
    for (let i = 0; i < 3; i++) {
      const ctx = await respondent(browser);
      const p = await ctx.newPage();
      await p.goto(s.url);
      await p.getByTestId('option').filter({ hasText: 'Bravo' }).click();
      await p.getByTestId('submit-vote').click();
      await ctx.close();
    }
    await page.goto(s.adminUrl);
    expect(await tally(page)).toEqual({ Alpha: 0, Bravo: 3, Charlie: 0 });
  });
});
