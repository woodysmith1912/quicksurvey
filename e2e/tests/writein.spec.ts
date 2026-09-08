import { expect, test } from '@playwright/test';
import { createSurvey, publish, respondent, signIn, tally, thumbsUp } from '../helpers';

test.describe('write-ins and moderation', () => {
  test('a suggestion is private until approved, then counts for its author', async ({ page, browser }) => {
    await signIn(page);
    const s = await createSurvey(page, 'Offsite ideas', ['Bowling']);
    await publish(page, s.adminUrl);

    const authorCtx = await respondent(browser);
    const author = await authorCtx.newPage();
    await author.goto(s.url);
    await thumbsUp(author, 'Bowling');
    await author.getByTestId('submit-vote').click();

    await author.getByTestId('writein-text').fill('Escape room');
    await author.getByTestId('submit-writein').click();
    await expect(author.getByTestId('flash')).toContainText('waiting for a moderator');

    // The author sees their own suggestion, marked, and their other choice is intact.
    await author.goto(s.url);
    const suggestion = author.getByTestId('option').filter({ hasText: 'Escape room' });
    await expect(suggestion).toContainText('awaiting review');
    await expect(author.getByTestId('option').filter({ hasText: 'Bowling' })).toBeVisible();

    // Nobody else sees it at all.
    const otherCtx = await respondent(browser);
    const other = await otherCtx.newPage();
    await other.goto(s.url);
    await expect(other.getByTestId('option').filter({ hasText: 'Escape room' })).toHaveCount(0);

    // It is not in the tally yet either.
    await page.goto(s.adminUrl);
    expect(await tally(page)).toEqual({ Bowling: 1 });

    // Approve it, rewording as we go.
    await expect(page.getByTestId('pending-item')).toHaveCount(1);
    await page.getByTestId('pending-text').fill('Escape room (downtown)');
    await page.getByTestId('approve').click();
    await expect(page.getByTestId('no-pending')).toBeVisible();

    // The author's vote carried over without them coming back.
    expect(await tally(page)).toEqual({ Bowling: 1, 'Escape room (downtown)': 1 });

    // And now everyone sees it, unmarked.
    await other.goto(s.url);
    await expect(other.getByTestId('option').filter({ hasText: 'Escape room (downtown)' })).toBeVisible();
    await expect(other.locator('body')).not.toContainText('awaiting review');

    await authorCtx.close();
    await otherCtx.close();
  });

  test('a rejected suggestion never appears or counts', async ({ page, browser }) => {
    await signIn(page);
    const s = await createSurvey(page, 'Rejection', ['Keep']);
    await publish(page, s.adminUrl);

    const ctx = await respondent(browser);
    const voter = await ctx.newPage();
    await voter.goto(s.url);
    await voter.getByTestId('writein-text').fill('Nonsense');
    await voter.getByTestId('submit-writein').click();

    await page.goto(s.adminUrl);
    await page.getByTestId('reject').click();
    await expect(page.getByTestId('no-pending')).toBeVisible();

    expect(await tally(page)).toEqual({ Keep: 0 });
    await voter.goto(s.url);
    await expect(voter.getByTestId('option').filter({ hasText: 'Nonsense' })).toHaveCount(0);
    await ctx.close();
  });

  test('merging a duplicate moves its votes without double counting', async ({ page, browser }) => {
    await signIn(page);
    const s = await createSurvey(page, 'Duplicate handling', ['Pizza']);
    await publish(page, s.adminUrl);

    // One person votes for Pizza and also suggests a duplicate of it.
    const bothCtx = await respondent(browser);
    const both = await bothCtx.newPage();
    await both.goto(s.url);
    await thumbsUp(both, 'Pizza');
    await both.getByTestId('submit-vote').click();
    await both.getByTestId('writein-text').fill('pizza!!!');
    await both.getByTestId('submit-writein').click();

    // A second person suggests the same thing and votes for nothing else.
    const soloCtx = await respondent(browser);
    const solo = await soloCtx.newPage();
    await solo.goto(s.url);
    await solo.getByTestId('writein-text').fill('PIZZA');
    await solo.getByTestId('submit-writein').click();

    await page.goto(s.adminUrl);
    await expect(page.getByTestId('pending-item')).toHaveCount(2);

    // Merge both suggestions into the original option.
    for (let i = 0; i < 2; i++) {
      const item = page.getByTestId('pending-item').first();
      await item.getByTestId('merge-target').selectOption({ label: 'Pizza' });
      await item.getByTestId('merge').click();
    }
    await expect(page.getByTestId('no-pending')).toBeVisible();

    // The person who voted for both counts once; the other adds one.
    expect(await tally(page)).toEqual({ Pizza: 2 });
    await expect(page.getByTestId('respondents')).toContainText('2 people have responded');

    await bothCtx.close();
    await soloCtx.close();
  });

  test('write-ins can be switched off per survey', async ({ page, browser }) => {
    await signIn(page);
    const s = await createSurvey(page, 'No suggestions please', ['Only this']);
    await publish(page, s.adminUrl);
    await page.goto(s.adminUrl + '/edit');
    await page.getByTestId('allow-writein').uncheck();
    await page.getByTestId('save-survey').click();

    const ctx = await respondent(browser);
    const voter = await ctx.newPage();
    await voter.goto(s.url);
    await expect(voter.getByTestId('writein-text')).toHaveCount(0);
    await ctx.close();
  });
});
