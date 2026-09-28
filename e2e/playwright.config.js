// @ts-check
const { defineConfig, devices } = require('@playwright/test');
const { execFileSync } = require('node:child_process');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');

// Workers load this file again. The env vars pin one port and one temp dir
// for the whole run, so every worker talks to the server webServer started.
if (!process.env.AB_E2E_PORT) {
  const probe = "const s=require('net').createServer();s.listen(0,'127.0.0.1',()=>{process.stdout.write(String(s.address().port));s.close()})";
  process.env.AB_E2E_PORT = execFileSync(process.execPath, ['-e', probe]).toString();
  process.env.AB_E2E_DIR = fs.mkdtempSync(path.join(os.tmpdir(), 'agentboard-e2e-'));
}
const port = process.env.AB_E2E_PORT;
const dir = process.env.AB_E2E_DIR;
const bin = path.join(dir, process.platform === 'win32' ? 'agentboard.exe' : 'agentboard');
const baseURL = `http://127.0.0.1:${port}`;

module.exports = defineConfig({
  testDir: '.',
  // One shared board: specs run one at a time so clear and filter counts hold.
  workers: 1,
  fullyParallel: false,
  retries: 0,
  use: { baseURL, trace: 'retain-on-failure' },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
  webServer: {
    command: `go build -o "${bin}" . && exec "${bin}" serve --listen 127.0.0.1:${port} --db "${path.join(dir, 'e2e.db')}"`,
    cwd: path.resolve(__dirname, '..'),
    url: `${baseURL}/api/board`,
    reuseExistingServer: false,
    timeout: 120_000,
  },
});
