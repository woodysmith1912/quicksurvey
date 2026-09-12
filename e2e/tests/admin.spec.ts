import { expect, test } from '@playwright/test';
import { createSurvey, publish, respondent, signIn, thumbsUp } from '../helpers';
import { ADMIN, BASE_URL } from '../env';
import { parseTsv } from '../helpers';

test.describe('administration', () => {
  test('signing in is required, and the destination is remembered', async ({ page }) => {
    await page.goto('/admin/');
    await expect(page).toHaveURL(/\/login/);
    await signIn(page);
    await expect(page).toHaveURL(/\/admin\//);
  });

  test('a wrong password is refused', async ({ page }) => {
    await page.goto('/login');
    await page.getByLabel('Username').fill(ADMIN.user);
    await page.getByLabel('Password').fill('definitely not it');
    await page.getByTestId('signin').click();
    await expect(page.getByTestId('flash')).toContainText('Incorrect');
    await expect(page.getByTestId('whoami')).toHaveCount(0);
  });

  test('editing renames, removes and adds options without losing votes', async ({ page, browser }) => {
    await signIn(page);
    const s = await createSurvey(page, 'Editable survey', ['Keep me', 'Remove me']);
    await publish(page, s.adminUrl);

    const ctx = await respondent(browser);
    const voter = await ctx.newPage();
    await voter.goto(s.url);
    await thumbsUp(voter, 'Keep me');
    await voter.getByTestId('submit-vote').click();

    await page.goto(s.adminUrl + '/edit');
    const editors = page.getByTestId('option-editor');
    await editors.nth(0).getByTestId('option-text').fill('Keep me (renamed)');
    await editors.nth(1).getByTestId('option-remove').check();
    await page.getByTestId('new-option-1').fill('Brand new');
    await page.getByTestId('save-survey').click();
    await expect(page.getByTestId('flash')).toContainText('Saved');

    // The rename kept the vote; the removal took the option off the ballot.
    await page.goto(s.adminUrl);
    await expect(page.getByTestId('tally-row')).toHaveCount(2);
    await expect(page.getByTestId('tally-row').filter({ hasText: 'Keep me (renamed)' }).getByTestId('votes'))
      .toHaveText('1');
    await expect(page.getByText('Not on the ballot')).toBeVisible();

    await voter.goto(s.url);
    await expect(voter.getByTestId('option').filter({ hasText: 'Remove me' })).toHaveCount(0);
    await expect(voter.getByTestId('option').filter({ hasText: 'Brand new' })).toBeVisible();
    await ctx.close();
  });

  test('the shareable link is shown and works', async ({ page, browser }) => {
    await signIn(page);
    const s = await createSurvey(page, 'Shareable', ['One']);
    await publish(page, s.adminUrl);
    const shown = (await page.getByTestId('share-url').innerText()).trim();
    expect(shown).toBe(`${BASE_URL}${s.url}`);

    const ctx = await respondent(browser);
    const anon = await ctx.newPage();
    await anon.goto(shown);
    await expect(anon.getByTestId('survey-title')).toHaveText('Shareable');
    await ctx.close();
  });

  test('a survey closes on its scheduled time without anyone touching it', async ({ page, browser }) => {
    await signIn(page);
    const s = await createSurvey(page, 'Auto closing', ['One']);
    await publish(page, s.adminUrl);

    // The server runs in UTC for the tests, so the local-time field is UTC too.
    const past = new Date(Date.now() - 60_000).toISOString().slice(0, 16);
    await page.goto(s.adminUrl + '/edit');
    await page.getByTestId('close-at').fill(past);
    await page.getByTestId('save-survey').click();

    const ctx = await respondent(browser);
    const anon = await ctx.newPage();
    await anon.goto(s.url);
    await expect(anon.getByTestId('closed')).toBeVisible();

    await page.goto(s.adminUrl);
    await expect(page.getByText('already past')).toBeVisible();
    await ctx.close();
  });

  test('deleting requires the title typed back', async ({ page }) => {
    await signIn(page);
    const s = await createSurvey(page, 'Doomed survey', ['One']);

    await page.goto(s.adminUrl);
    await page.getByTestId('delete-confirm').fill('wrong text');
    await page.getByTestId('delete-survey').click();
    await expect(page.getByTestId('flash')).toContainText('did not match');

    await page.getByTestId('delete-confirm').fill('Doomed survey');
    await page.getByTestId('delete-survey').click();
    await expect(page).toHaveURL(/\/admin\/$/);
    await expect(page.getByTestId('survey-row').filter({ hasText: 'Doomed survey' })).toHaveCount(0);

    await page.goto(s.adminUrl);
    await expect(page.getByTestId('error')).toBeVisible();
  });

  test('exports download as TSV that matches the tally', async ({ page, browser }) => {
    await signIn(page);
    const s = await createSurvey(page, 'Export me', ['Alpha', 'Beta']);
    await publish(page, s.adminUrl);

    const ctx = await respondent(browser);
    const voter = await ctx.newPage();
    await voter.goto(s.url);
    await thumbsUp(voter, 'Alpha');
    await voter.getByTestId('comment').fill('tab\there');
    await voter.getByTestId('submit-vote').click();
    await ctx.close();

    await page.goto(s.adminUrl);

    const responses = await downloadText(page, 'export-responses');
    const r = parseTsv(responses);
    expect(r.header).toEqual([
      'response_id', 'submitted', 'updated', 'Alpha', 'Beta', 'selections', 'comment',
    ]);
    expect(r.rows).toHaveLength(1);
    expect(r.rows[0].Alpha).toBe('1');
    expect(r.rows[0].Beta).toBe('0');
    expect(r.rows[0].selections).toBe('1');
    // The tab the respondent typed must not have shifted a column.
    expect(r.rows[0].comment).toBe('tab here');

    const summary = parseTsv(await downloadText(page, 'export-summary'));
    expect(summary.header).toEqual([
      'option', 'votes', 'respondents', 'percent_of_respondents', 'shown_to', 'percent_of_shown',
    ]);
    const alpha = summary.rows.find((row) => row.option === 'Alpha')!;
    expect(alpha.votes).toBe('1');
    expect(alpha.percent_of_respondents).toBe('100.0');
    expect(alpha.shown_to).toBe('1');
    expect(alpha.percent_of_shown).toBe('100.0');
  });
});

/** Clicks a download link and returns the file's contents as text. */
async function downloadText(page: import('@playwright/test').Page, testId: string) {
  const [download] = await Promise.all([
    page.waitForEvent('download'),
    page.getByTestId(testId).click(),
  ]);
  expect(download.suggestedFilename()).toMatch(/\.tsv$/);
  const stream = await download.createReadStream();
  const chunks: Buffer[] = [];
  for await (const chunk of stream) chunks.push(Buffer.from(chunk));
  return Buffer.concat(chunks).toString('utf8');
}
