// @ts-check
const { test, expect, sel, uid, openBoard } = require('./fixtures');

test('Clear finished removes ended sessions but keeps one with an open decision', async ({ page, api }) => {
  // Start from a board with no clearable ended sessions left by other specs.
  await api.ui('/ui/clear', { mode: 'ended_all' });

  const ctx = (tag) => ({ session: `e2e-clr-${tag}-${uid()}`, machine: 'devbox', project: 'relay', name: tag });
  const [ended1, ended2, endedOpen, active] = ['e1', 'e2', 'eopen', 'act'].map(ctx);
  for (const c of [ended1, ended2, endedOpen]) {
    await api.hook(c, 'SessionStart');
    await api.hook(c, 'SessionEnd', { reason: 'logout' });
  }
  await api.ask(endedOpen, 'Keep this?', 'yes');
  await api.hook(active, 'SessionStart');
  await api.hook(active, 'UserPromptSubmit');

  await openBoard(page);
  await page.locator(sel('clear-open')).click();
  const dialog = page.locator('dialog[data-testid="clear-dialog"]');
  await expect(dialog).toBeVisible();
  await expect(dialog.locator(sel('clear-count', { mode: 'ended_all' }))).toHaveText(/\b2\b/);
  await expect(dialog.locator(sel('clear-count', { mode: 'ended_older' }))).toHaveText(/\b0\b/);
  await expect(dialog.locator(sel('clear-count', { mode: 'quiet' }))).toHaveText(/\b0\b/);

  await dialog.locator('input[name="clear-mode"][value="ended_all"]').check();
  await dialog.locator(sel('clear-confirm')).click();

  const row = (c) => page.locator(sel('session-row', { session: c.session }));
  await expect(row(ended1)).toHaveCount(0);
  await expect(row(ended2)).toHaveCount(0);
  await expect(row(endedOpen)).toBeVisible();
  await expect(row(active)).toBeVisible();
  await expect(dialog).toBeHidden();
});

test('Delete this session removes it after confirmation', async ({ page, api }) => {
  const c = { session: `e2e-del-${uid()}`, machine: 'devbox', project: 'relay', name: 'p-del' };
  await api.hook(c, 'SessionStart');
  await openBoard(page);

  await page.locator(`${sel('session-row', { session: c.session })} a[data-testid="session-link"]`).click();
  await expect(page.locator(sel('session-view', { session: c.session }))).toBeVisible();
  await page.locator(sel('session-delete')).click();
  const dialog = page.locator('dialog[data-testid="delete-dialog"]');
  await expect(dialog).toBeVisible();
  await dialog.locator(sel('delete-confirm')).click();

  await expect.poll(async () => (await page.request.get(`/api/sessions/${c.session}`)).status()).toBe(404);
  await page.goto('/');
  await expect(page.locator(sel('live'))).toHaveAttribute('data-state', 'live');
  await expect(page.locator(sel('session-row', { session: c.session }))).toHaveCount(0);
});
