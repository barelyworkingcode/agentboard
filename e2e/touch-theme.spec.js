// @ts-check
const { test, expect, sel, uid, openBoard } = require('./fixtures');

const TABLET = [{ width: 744, height: 1133 }, { width: 1133, height: 744 }];
const PHONE = { width: 390, height: 844 };
const KEY = 'agentboard-theme';
const CHOICES = ['light', 'dark', 'auto'];

const LONG = {
  branch: 'feat/14-make-the-board-touch-friendly-and-wrap-long-table-cells-on-tablets'.slice(0, 60),
  project: 'acme-observability-platform-and-reliability-tooling-monorepo-for-the-testbox-fleet',
  note: 'rebuilding the ledger index after the schema change, then re-running the tablet layout checks on both '
    + 'orientations before asking for a review of the wrapped table cells',
  title: 'Make the board touch friendly on small tablets, add a light and dark and auto theme switch to the header, '
    + 'and wrap every long table cell so nothing scrolls sideways',
};

/**
 * A session and a ledger item carrying long values, plus a second session with an open decision
 * (asking would put the first one in "waiting on you" and hide its note).
 */
async function seed(api) {
  const ctx = { session: `e2e-touch-${uid()}`, machine: 'testbox', project: LONG.project, branch: LONG.branch,
    repo: 'acme/relay', issue: 14, name: 'p-touch' };
  await api.hook(ctx, 'SessionStart');
  await api.post('/api/state', { ctx, note: LONG.note });
  await api.post('/api/items', { ctx, repo: 'acme/relay', number: 14, title: LONG.title, pr: 15, state: 'waiting', tier: 'review' });
  const asker = { ...ctx, session: `e2e-touch-ask-${uid()}`, name: 'p-ask' };
  await api.hook(asker, 'SessionStart');
  await api.ask(asker, 'Merge the tablet layout?', 'yes, CI is green');
  return { ctx, asker };
}

async function openSession(page, id) {
  await page.goto(`/?session=${encodeURIComponent(id)}`);
  await expect(page.locator(sel('session-view', { session: id }))).toBeVisible();
}

/** Every element matching each selector, reported when narrower or shorter than 44 CSS px. */
async function expectTappable(page, selectors) {
  const small = await page.evaluate((sels) => sels.flatMap((s) => {
    const els = [...document.querySelectorAll(s)];
    if (!els.length) return [`${s}: none found`];
    return els.map((el) => [el, el.getBoundingClientRect()])
      .filter(([, r]) => r.width < 43.99 || r.height < 43.99)
      .map(([el, r]) => `${s} "${el.textContent.trim().slice(0, 30)}" ${r.width.toFixed(1)}x${r.height.toFixed(1)}`);
  }), selectors);
  expect(small).toEqual([]);
}

async function expectNoSideScroll(page) {
  expect(await page.evaluate(() => document.documentElement.scrollWidth - innerWidth)).toBeLessThanOrEqual(0);
}

/** Measures every table-cell occurrence of value: line count, clipping, ellipsis, and viewport overflow. */
function measureInCells(page, value) {
  return page.evaluate((v) => {
    const out = [];
    const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
    for (let n = walker.nextNode(); n; n = walker.nextNode()) {
      const td = n.parentElement && n.parentElement.closest('td');
      const at = n.data.indexOf(v);
      if (!td || at < 0) continue;
      const range = document.createRange();
      range.setStart(n, at);
      range.setEnd(n, at + v.length);
      const rects = [...range.getClientRects()].filter((r) => r.width > 0);
      const cell = td.getBoundingClientRect();
      out.push({
        lines: new Set(rects.map((r) => Math.round(r.top))).size,
        clipped: rects.some((r) => r.left < cell.left - 1 || r.right > cell.right + 1) || td.scrollWidth > td.clientWidth + 1,
        ellipsis: [n.parentElement, td].some((e) => getComputedStyle(e).textOverflow === 'ellipsis'),
        wider: cell.right > innerWidth || cell.left < 0,
      });
    }
    return out;
  }, value);
}

async function expectWrapped(page, values) {
  for (const [label, value] of values) {
    const cells = await measureInCells(page, value);
    expect(cells.length, `${label} in a table cell`).toBeGreaterThan(0);
    for (const m of cells) {
      expect(m.lines, `${label} wraps`).toBeGreaterThan(1);
      expect(m, label).toMatchObject({ clipped: false, ellipsis: false, wider: false });
    }
  }
}

/** Tone of the page background: dark, light, or none before any stylesheet paints it. */
function tone(page) {
  return page.evaluate(() => {
    const c = (getComputedStyle(document.documentElement).backgroundColor.match(/[\d.]+/g) || []).map(Number);
    if (c.length < 3 || c[3] === 0) return 'none';
    return (0.2126 * c[0] + 0.7152 * c[1] + 0.0722 * c[2]) / 255 < 0.5 ? 'dark' : 'light';
  });
}

async function expectChoice(page, choice) {
  for (const t of CHOICES) await expect(page.locator(sel(`theme-${t}`))).toHaveAttribute('aria-pressed', String(t === choice));
  if (choice === 'auto') await expect(page.locator('html')).not.toHaveAttribute('data-theme', /./);
  else await expect(page.locator('html')).toHaveAttribute('data-theme', choice);
}

const storeTheme = (page, value) => page.addInitScript(([k, v]) => {
  try { localStorage.setItem(k, v); } catch { /* about:blank has no storage */ }
}, [KEY, value]);

for (const vp of TABLET) {
  const at = `at ${vp.width}x${vp.height}`;
  test.describe(at, () => {
    test.use({ viewport: vp });

    test('board: chips, buttons, dialog controls and table and who-line links are 44 px targets', async ({ page, api }) => {
      await seed(api);
      await openBoard(page);
      await expectTappable(page, [sel('chip'), `${sel('theme-switch')} button`, sel('clear-open'), sel('decision-dismiss'),
        `${sel('session-row')} a`, `${sel('item-row')} a`, `${sel('decision')} a`]);
      await page.locator(sel('clear-open')).click();
      await expect(page.locator(sel('clear-dialog'))).toBeVisible();
      await expectTappable(page, [`${sel('clear-dialog')} label`, sel('clear-cancel'), sel('clear-confirm')]);
    });

    test('session view: theme control, buttons and dialog controls are 44 px targets', async ({ page, api }) => {
      const { asker } = await seed(api);
      await openSession(page, asker.session);
      await expectTappable(page, [`${sel('theme-switch')} button`, sel('decision-dismiss'), sel('session-delete')]);
      await page.locator(sel('session-delete')).click();
      await expect(page.locator(sel('delete-dialog'))).toBeVisible();
      await expectTappable(page, [sel('delete-cancel'), sel('delete-confirm')]);
    });

    test('long branch, project, note and title wrap inside their cells', async ({ page, api }) => {
      const { ctx } = await seed(api);
      await openBoard(page);
      await expectWrapped(page, [['branch', LONG.branch], ['project', LONG.project], ['note', LONG.note], ['title', LONG.title]]);
      await openSession(page, ctx.session);
      await expectWrapped(page, [['history note', LONG.note]]);
    });

    test('the chosen theme persists under one storage key across reloads', async ({ page }) => {
      await openBoard(page);
      await expectChoice(page, 'auto');
      for (const choice of CHOICES) {
        await page.locator(sel(`theme-${choice}`)).click();
        await expectChoice(page, choice);
        await page.reload();
        await expectChoice(page, choice);
      }
      await page.locator(sel('theme-dark')).click();
      expect(await page.evaluate(() => ({ ...localStorage }))).toEqual({ [KEY]: 'dark' });
    });

    for (const [label, value] of [['missing', null], ['invalid', 'sepia']]) {
      test(`a ${label} stored theme means Auto`, async ({ page }) => {
        if (value !== null) await storeTheme(page, value);
        await openBoard(page);
        await expectChoice(page, 'auto');
      });
    }

    const THROWING = {
      'reading storage throws': () => Object.defineProperty(window, 'localStorage', {
        configurable: true, get() { throw new DOMException('storage disabled', 'SecurityError'); },
      }),
      'writing storage throws': () => {
        const full = () => { throw new DOMException('storage full', 'QuotaExceededError'); };
        Storage.prototype.setItem = full;
        Storage.prototype.removeItem = full;
      },
    };
    for (const [label, script] of Object.entries(THROWING)) {
      test(`the switch still works for the session when ${label}`, async ({ page }) => {
        await page.emulateMedia({ colorScheme: 'light' });
        await page.addInitScript(script);
        await openBoard(page);
        await expectChoice(page, 'auto');
        for (const [choice, want] of [['dark', 'dark'], ['light', 'light'], ['auto', 'light']]) {
          await page.locator(sel(`theme-${choice}`)).click();
          await expectChoice(page, choice);
          expect(await tone(page)).toBe(want);
        }
      });
    }

    for (const choice of CHOICES) {
      test(`${choice} ${choice === 'auto' ? 'follows' : 'ignores'} an OS scheme change without reload`, async ({ page }) => {
        await page.emulateMedia({ colorScheme: 'light' });
        await openBoard(page);
        await page.locator(sel(`theme-${choice}`)).click();
        await page.evaluate(() => { window.__sameDocument = true; });
        for (const os of ['dark', 'light', 'dark']) {
          await page.emulateMedia({ colorScheme: os });
          await expect.poll(() => tone(page)).toBe(choice === 'auto' ? os : choice);
        }
        expect(await page.evaluate(() => window.__sameDocument)).toBe(true);
      });
    }

    test('stored Dark with OS Light paints dark before app.js runs', async ({ page }) => {
      await page.emulateMedia({ colorScheme: 'light' });
      await storeTheme(page, 'dark');
      let release = () => {};
      const held = new Promise((r) => { release = r; });
      await page.route('**/app.js', async (route) => { await held; await route.continue(); });
      await page.goto('/', { waitUntil: 'commit' });
      // Once the stylesheet paints the background, the script-free page must already be dark.
      await expect.poll(() => tone(page)).not.toBe('none');
      expect(await tone(page)).toBe('dark');
      release();
      await expectChoice(page, 'dark');
      expect(await tone(page)).toBe('dark');
    });
  });
}

for (const vp of [...TABLET, PHONE]) {
  test.describe(`at ${vp.width}px`, () => {
    test.use({ viewport: vp });

    test('board and session view have no horizontal scroll', async ({ page, api }) => {
      const { ctx } = await seed(api);
      await openBoard(page);
      await expect(page.locator(sel('session-row', { session: ctx.session }))).toBeVisible();
      await expectNoSideScroll(page);
      await openSession(page, ctx.session);
      await expectNoSideScroll(page);
    });
  });
}
