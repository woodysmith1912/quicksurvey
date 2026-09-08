import { expect, test } from '@playwright/test';
import { createSurvey, publish, respondent, signIn, tally, thumbsUp } from '../helpers';

test.describe('taking a survey', () => {
  test('a draft is not reachable until it is published', async ({ page, browser }) => {
    await signIn(page);
    const s = await createSurvey(page, 'Draft only', ['A', 'B']);

    const visitor = await respondent(browser);
    const anon = await visitor.newPage();
    await anon.goto(s.url);
    await expect(anon.getByTestId('error')).toBeVisible();
    await visitor.close();

    // The editor, however, gets a clearly marked preview.
    await page.goto(s.url);
    await expect(page.getByText('Preview')).toBeVisible();

    await publish(page, s.adminUrl);
    const after = await respondent(browser);
    const anon2 = await after.newPage();
    await anon2.goto(s.url);
    await expect(anon2.getByTestId('survey-title')).toHaveText('Draft only');
    await after.close();
  });

  test('thumbs-up any number of options, then change the answer', async ({ page, browser }) => {
    await signIn(page);
    const s = await createSurvey(page, 'Lunch spots', ['Pizza', 'Tacos', 'Salad']);
    await publish(page, s.adminUrl);

    const ctx = await respondent(browser);
    const voter = await ctx.newPage();
    await voter.goto(s.url);

    // Nothing is preselected on a first visit.
    for (const box of await voter.locator('ul.ballot input[type=checkbox]').all()) {
      await expect(box).not.toBeChecked();
    }

    await thumbsUp(voter, 'Pizza', 'Salad');
    await voter.getByTestId('comment').fill('no anchovies');
    await voter.getByTestId('submit-vote').click();
    await expect(voter.getByTestId('flash')).toContainText('recorded');

    // Returning shows the previous answer, and the button says so.
    await voter.goto(s.url);
    await expect(voter.locator('#opt-' + (await optionId(voter, 'Pizza')))).toBeChecked();
    await expect(voter.getByTestId('comment')).toHaveValue('no anchovies');
    await expect(voter.getByTestId('submit-vote')).toHaveText(/Update/);

    // Changing it replaces rather than adds.
    await thumbsUp(voter, 'Pizza');   // untick
    await thumbsUp(voter, 'Tacos');   // tick
    await voter.getByTestId('submit-vote').click();

    await page.goto(s.adminUrl);
    expect(await tally(page)).toEqual({ Pizza: 0, Tacos: 1, Salad: 1 });
    await expect(page.getByTestId('respondents')).toContainText('1 person has responded');
    await ctx.close();
  });

  test('one browser counts once, a different browser counts separately', async ({ page, browser }) => {
    await signIn(page);
    const s = await createSurvey(page, 'Repeat voting', ['Yes', 'No']);
    await publish(page, s.adminUrl);

    const one = await respondent(browser);
    const p1 = await one.newPage();
    for (let i = 0; i < 3; i++) {
      await p1.goto(s.url);
      await thumbsUp(p1, 'Yes');
      await p1.getByTestId('submit-vote').click();
    }

    await page.goto(s.adminUrl);
    await expect(page.getByTestId('respondents')).toContainText('1 person has responded');

    const two = await respondent(browser);
    const p2 = await two.newPage();
    await p2.goto(s.url);
    await thumbsUp(p2, 'Yes');
    await p2.getByTestId('submit-vote').click();

    await page.goto(s.adminUrl);
    await expect(page.getByTestId('respondents')).toContainText('2 people have responded');
    expect(await tally(page)).toEqual({ Yes: 2, No: 0 });

    await one.close();
    await two.close();
  });

  test('respondents see the tally only when the editor turns it on', async ({ page, browser }) => {
    await signIn(page);
    const s = await createSurvey(page, 'Shared results', ['Alpha', 'Beta']);
    await publish(page, s.adminUrl);

    const ctx = await respondent(browser);
    const voter = await ctx.newPage();
    await voter.goto(s.url);
    await expect(voter.getByTestId('tally')).toHaveCount(0);
    await voter.goto(s.url + '/results');
    await expect(voter.getByTestId('error')).toBeVisible();

    await page.goto(s.adminUrl + '/edit');
    await page.getByTestId('show-results').check();
    await page.getByTestId('save-survey').click();

    await voter.goto(s.url);
    await expect(voter.getByTestId('tally')).toBeVisible();
    await ctx.close();
  });

  test('one respondent never sees another respondent\'s comment', async ({ page, browser }) => {
    await signIn(page);
    const s = await createSurvey(page, 'Comment privacy', ['Alpha']);
    await publish(page, s.adminUrl);
    await page.goto(s.adminUrl + '/edit');
    await page.getByTestId('show-results').check();
    await page.getByTestId('save-survey').click();

    const secret = 'strictly between us';
    const authorCtx = await respondent(browser);
    const author = await authorCtx.newPage();
    await author.goto(s.url);
    await thumbsUp(author, 'Alpha');
    await author.getByTestId('comment').fill(secret);
    await author.getByTestId('submit-vote').click();

    // The author gets their own comment back, because they can still edit it.
    await author.goto(s.url);
    await expect(author.getByTestId('comment')).toHaveValue(secret);

    // Nobody else sees it — not on the ballot, not on the shared results.
    const otherCtx = await respondent(browser);
    const other = await otherCtx.newPage();
    await other.goto(s.url);
    await expect(other.locator('body')).not.toContainText(secret);
    await expect(other.getByTestId('comment')).toHaveValue('');
    await other.goto(s.url + '/results');
    await expect(other.locator('body')).not.toContainText(secret);

    // Signed-in accounts do see it.
    await page.goto(s.adminUrl);
    await expect(page.getByTestId('comments')).toContainText(secret);

    await authorCtx.close();
    await otherCtx.close();
  });

  test('a closed survey says so and takes no more votes', async ({ page, browser }) => {
    await signIn(page);
    const s = await createSurvey(page, 'Closing time', ['Alpha']);
    await publish(page, s.adminUrl);

    const ctx = await respondent(browser);
    const voter = await ctx.newPage();
    await voter.goto(s.url);
    await thumbsUp(voter, 'Alpha');
    await voter.getByTestId('submit-vote').click();

    await page.goto(s.adminUrl);
    await page.getByTestId('close-survey').click();
    await expect(page.getByTestId('state')).toHaveText('closed');

    await voter.goto(s.url);
    await expect(voter.getByTestId('closed')).toBeVisible();
    await expect(voter.getByTestId('submit-vote')).toHaveCount(0);

    await page.goto(s.adminUrl);
    expect(await tally(page)).toEqual({ Alpha: 1 });
    await ctx.close();
  });
});

/** Reads the option ID out of a checkbox whose label matches the given text. */
async function optionId(page: import('@playwright/test').Page, label: string) {
  const forAttr = await page.getByTestId('option').filter({ hasText: label }).first().getAttribute('for');
  return forAttr!.replace(/^opt-/, '');
}
