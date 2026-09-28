// @ts-check
const { test, expect, sel, uid, openBoard } = require('./fixtures');

const LIVE = { timeout: 2000 };

test('a new session shows within 2 s with its context and issue link', async ({ page, api }) => {
  await openBoard(page);
  const id = `e2e-new-${uid()}`;
  await api.hook({ session: id, machine: 'devbox', project: 'relay', branch: 'feat/88-x', repo: 'acme/relay', issue: 88, name: 'p1' },
    'SessionStart', { source: 'startup' });

  const row = page.locator(sel('session-row', { session: id }));
  await expect(row).toBeVisible(LIVE);
  await expect(row.locator('a[href="https://github.com/acme/relay/issues/88"]')).toHaveText('relay#88');
  for (const text of ['devbox', 'relay', 'feat/88-x', 'p1']) await expect(row).toContainText(text);
  await expect(row).toHaveAttribute('data-display', 'idle');
});

test('a permission prompt lists the session under What needs you until the next tool use', async ({ page, api }) => {
  await openBoard(page);
  const ctx = { session: `e2e-wait-${uid()}`, machine: 'devbox', project: 'relay', name: 'p2' };
  await api.hook(ctx, 'SessionStart');
  await api.hook(ctx, 'PreToolUse', { tool: 'Bash', tool_use_id: 'tu1' });
  await api.hook(ctx, 'Notification', { notification_type: 'permission_prompt' });

  const needs = page.locator(`${sel('needs-group', { kind: 'waiting' })} ${sel('needs-session', { session: ctx.session })}`);
  await expect(needs).toBeVisible(LIVE);
  await expect(page.locator(sel('session-row', { session: ctx.session }))).toHaveAttribute('data-display', 'waiting');

  await api.hook(ctx, 'PreToolUse', { tool: 'Read', tool_use_id: 'tu2' });
  await expect(page.locator(sel('needs-session', { session: ctx.session }))).toHaveCount(0, LIVE);
  await expect(page.locator(sel('session-row', { session: ctx.session }))).toHaveAttribute('data-display', 'active');
});

test('a decision shows its rec and context; answer and dismiss remove it', async ({ page, api }) => {
  await openBoard(page);
  const ctx = { session: `e2e-ask-${uid()}`, machine: 'devbox', project: 'relay', branch: 'fix/7-y', repo: 'acme/relay', issue: 7, name: 'p-ask' };
  await api.hook(ctx, 'SessionStart');
  const asked = await api.ask(ctx, 'Ship the fix?', 'yes, CI is green');
  const review = await api.ask(ctx, 'Review the diff?', 'merge it', 'review');

  const card = page.locator(`${sel('needs-group', { kind: 'question' })} ${sel('decision', { id: asked.id })}`);
  await expect(card).toBeVisible(LIVE);
  await expect(card).toContainText('Ship the fix?');
  await expect(card.locator(sel('decision-rec'))).toContainText('yes, CI is green');
  for (const text of ['devbox', 'p-ask', 'fix/7-y']) await expect(card).toContainText(text);
  await expect(page.locator(`${sel('needs-group', { kind: 'review' })} ${sel('decision', { id: review.id })}`)).toBeVisible();

  await api.post(`/api/decisions/${asked.id}/answer`, { ctx, text: 'go' });
  await expect(page.locator(sel('decision', { id: asked.id }))).toHaveCount(0, LIVE);

  await page.locator(`${sel('decision', { id: review.id })} ${sel('decision-dismiss')}`).click();
  await expect(page.locator(sel('decision', { id: review.id }))).toHaveCount(0, LIVE);
  expect((await api.board()).decisions.map((d) => d.id)).not.toContain(review.id);
});

test('quiet and in-tool sessions render from the board payload', async ({ page }) => {
  const now = Date.now();
  const min = 60_000;
  const ctx = { machine: 'devbox', project: 'relay', branch: 'main', repo: '', issue: 0, name: '', run: '' };
  const row = (id, extra) => ({
    id, ctx: { ...ctx, session: id, name: id }, state: 'active', waiting_on: '', tool: '', tool_since: 0,
    last_seen_at: now - 34 * min, ended_at: 0, display: 'active', note: '', pr: 0, started_at: now - 60 * min, ...extra,
  });
  const board = {
    version: 'fixture.1', now,
    sessions: [
      row('fx-quiet', { display: 'quiet' }),
      row('fx-tool', { tool: 'Bash', tool_since: now - 45 * min, last_seen_at: now - 45 * min }),
    ],
    decisions: [], runs: [], meters: [], items: [], notes: [], log: [],
  };
  await page.route('**/api/board', (route) => route.fulfill({ json: board }));
  await page.goto('/');

  const quiet = page.locator(sel('session-row', { session: 'fx-quiet' }));
  await expect(quiet).toHaveAttribute('data-display', 'quiet');
  await expect(quiet).toContainText(/quiet \d+/);
  const inTool = page.locator(sel('session-row', { session: 'fx-tool' }));
  await expect(inTool).toHaveAttribute('data-display', 'active');
  await expect(inTool).toContainText(/running Bash since \d{1,2}:\d{2}/);
  await expect(inTool).not.toContainText('quiet');
});
