// @ts-check
const { test, expect, sel, uid, openBoard } = require('./fixtures');

test('a posted log line shows within 2 s without a reload', async ({ page, api }) => {
  await openBoard(page);
  await page.evaluate(() => { window.__sameDocument = true; });
  const text = `e2e live line ${uid()}`;
  await api.post('/api/log', { ctx: { machine: 'devbox', project: 'relay' }, text });

  await expect(page.locator(sel('log-line')).filter({ hasText: text })).toBeVisible({ timeout: 2000 });
  expect(await page.evaluate(() => window.__sameDocument), 'the page reloaded itself').toBe(true);
});

test.describe('filter chips', () => {
  // A and B share a project, B and C share a machine, C is ad hoc.
  const records = {
    A: { machine: 'e2e-m1', project: 'e2e-alpha', run: 'e2e-r1', number: 1 },
    B: { machine: 'e2e-m2', project: 'e2e-alpha', run: 'e2e-r2', number: 2 },
    C: { machine: 'e2e-m2', project: 'e2e-beta', run: '', number: 3 },
  };
  const seeded = {};

  test.beforeAll(async ({ request, baseURL }) => {
    const post = async (path, data) => {
      const r = await request.post(`${baseURL}${path}`, { data });
      expect(r.ok(), `${path} -> ${r.status()}`).toBeTruthy();
      return r.status() === 204 ? null : r.json();
    };
    for (const [k, r] of Object.entries(records)) {
      const ctx = { machine: r.machine, project: r.project, run: r.run, session: `e2e-f-${k}-${uid()}`, name: `f-${k}` };
      await post('/api/hook', { ctx, event: 'SessionStart' });
      const { id } = await post('/api/decisions', { ctx, kind: 'question', question: `filter-q-${k}`, rec: 'ok' });
      await post('/api/log', { ctx, text: `filter-log-${k}` });
      await post('/api/notes', { ctx, kind: 'well', text: `filter-note-${k}` });
      const { key } = await post('/api/items', { ctx, repo: `acme/${r.project}`, number: r.number, title: `filter-item-${k}` });
      seeded[k] = { session: ctx.session, decision: id, key };
    }
  });

  /** One locator per board section for a seeded record. */
  const sections = (page, k) => ({
    session: page.locator(sel('session-row', { session: seeded[k].session })),
    decision: page.locator(sel('decision', { id: seeded[k].decision })),
    log: page.locator(sel('log-line')).filter({ hasText: `filter-log-${k}` }),
    note: page.locator(sel('note')).filter({ hasText: `filter-note-${k}` }),
    item: page.locator(sel('item-row', { key: seeded[k].key })),
  });

  async function expectNarrowed(page, shown) {
    for (const k of Object.keys(records)) {
      for (const [name, loc] of Object.entries(sections(page, k))) {
        if (shown.includes(k)) await expect(loc, `${name} ${k} should show`).toBeVisible();
        else await expect(loc, `${name} ${k} should be hidden`).toBeHidden();
      }
    }
  }

  const cases = [
    { dim: 'machine', value: 'e2e-m1', shown: ['A'] },
    { dim: 'project', value: 'e2e-alpha', shown: ['A', 'B'] },
    { dim: 'run', value: 'e2e-r2', shown: ['B'] },
    { dim: 'run', value: '_adhoc', shown: ['C'] },
  ];
  for (const c of cases) {
    test(`${c.dim}=${c.value} narrows every section and survives a reload`, async ({ page }) => {
      await page.goto('/');
      await expectNarrowed(page, ['A', 'B', 'C']);
      const chip = page.locator(sel('chip', { dim: c.dim, value: c.value }));
      await chip.click();
      await expect(chip).toHaveAttribute('aria-pressed', 'true');
      expect(new URL(page.url()).searchParams.get(c.dim)).toBe(c.value);
      await expectNarrowed(page, c.shown);

      await page.reload();
      await expect(page.locator(sel('chip', { dim: c.dim, value: c.value }))).toHaveAttribute('aria-pressed', 'true');
      await expectNarrowed(page, c.shown);
    });
  }
});
