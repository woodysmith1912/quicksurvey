import { expect, test } from '@playwright/test';
import { createSurvey, publish, respondent, signIn, tally, thumbsUp } from '../helpers';

test.describe('draft preview', () => {
  test('an editor can take a draft, and publishing throws the test data away', async ({ page, browser }) => {
    await signIn(page);
    const s = await createSurvey(page, 'Draft I can try', ['Pizza', 'Tacos']);

    // The admin page offers the preview explicitly.
    await page.goto(s.adminUrl);
    await expect(page.getByTestId('state')).toHaveText('draft');
    await expect(page.getByTestId('take-survey')).toHaveText(/Preview and try it/);
    await page.getByTestId('take-survey').click();

    // It is a real, usable ballot — not a "closed" notice.
    await expect(page.getByTestId('preview-banner')).toBeVisible();
    await expect(page.getByTestId('closed')).toHaveCount(0);
    await expect(page.getByTestId('submit-vote')).toBeVisible();

    await thumbsUp(page, 'Pizza');
    await page.getByTestId('comment').fill('checking the wording');
    await page.getByTestId('submit-vote').click();
    await expect(page.getByTestId('flash')).toContainText('test response');

    // The test vote is visible while still a draft.
    await page.goto(s.adminUrl);
    expect(await tally(page)).toEqual({ Pizza: 1, Tacos: 0 });

    // Nobody else can reach the draft at all.
    const ctx = await respondent(browser);
    const anon = await ctx.newPage();
    await anon.goto(s.url);
    await expect(anon.getByTestId('error')).toBeVisible();

    // Publishing discards it and says so.
    await publish(page, s.adminUrl);
    await expect(page.getByTestId('flash')).toContainText('discarded');
    expect(await tally(page)).toEqual({ Pizza: 0, Tacos: 0 });
    await expect(page.getByTestId('respondents')).toContainText('0');

    // Real responses now behave normally and survive a close/reopen.
    await anon.goto(s.url);
    await thumbsUp(anon, 'Tacos');
    await anon.getByTestId('submit-vote').click();
    await page.goto(s.adminUrl);
    await page.getByTestId('close-survey').click();
    await page.getByTestId('open-survey').click();
    expect(await tally(page)).toEqual({ Pizza: 0, Tacos: 1 });
    await ctx.close();
  });

  test('write-ins work in a draft preview too', async ({ page }) => {
    await signIn(page);
    const s = await createSurvey(page, 'Draft write-ins', ['Only this']);
    await page.goto(s.url);
    await page.getByTestId('writein-text').fill('Suggested while drafting');
    await page.getByTestId('submit-writein').click();
    await page.goto(s.adminUrl);
    await expect(page.getByTestId('pending-item')).toHaveCount(1);
  });
});
