// @ts-check
const { test: base, expect } = require('@playwright/test');

/** CSS selector for a data-testid plus data-* attributes. */
function sel(testid, attrs = {}) {
  return `[data-testid="${testid}"]` + Object.entries(attrs).map(([k, v]) => `[data-${k}="${v}"]`).join('');
}

const uid = () => Math.random().toString(36).slice(2, 8);

const test = base.extend({
  /** Seeds the board through the ingest API, as an agent would. */
  api: async ({ request, baseURL }, use) => {
    async function post(path, data, headers = {}) {
      const r = await request.post(path, { data, headers });
      expect(r.ok(), `POST ${path} -> ${r.status()} ${await r.text()}`).toBeTruthy();
      return r.status() === 204 ? null : r.json();
    }
    await use({
      post,
      hook: (ctx, event, extra = {}) => post('/api/hook', { ctx, event, ...extra }),
      ask: (ctx, question, rec, kind = 'question') => post('/api/decisions', { ctx, kind, question, rec }),
      /** A browser action, sent the way the page sends it. */
      ui: (path, data) => post(path, data, { Origin: baseURL, 'X-Agentboard': '1' }),
      board: async () => (await request.get('/api/board')).json(),
    });
  },
});

/** Opens the board and waits for the live stream, so later posts test push. */
async function openBoard(page) {
  await page.goto('/');
  await expect(page.locator(sel('live'))).toHaveAttribute('data-state', 'live');
}

module.exports = { test, expect, sel, uid, openBoard };
