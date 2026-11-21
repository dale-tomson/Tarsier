'use strict';

const { execFileSync } = require('child_process');
const fs = require('fs');
const path = require('path');

function loadPlaywright() {
  const candidates = [
    'playwright',
    'playwright-core',
    '/usr/lib/node_modules/playwright',
    '/usr/local/lib/node_modules/playwright',
    '/tmp/pw/node_modules/playwright-core',
    '/tmp/pw/node_modules/playwright',
  ];
  for (const candidate of candidates) {
    try {
      return require(candidate);
    } catch (_) {
      // Try next candidate
    }
  }

  let version = '1.46.1';
  try {
    const info = JSON.parse(fs.readFileSync('/ms-playwright/.docker-info', 'utf8'));
    if (info && info.driverVersion) {
      version = info.driverVersion;
    }
  } catch (_) {}

  console.log(`Installing playwright-core@${version} inside container (/tmp/pw)...`);
  fs.mkdirSync('/tmp/pw', { recursive: true });
  execFileSync(
    'npm',
    ['install', '--no-save', '--no-audit', '--no-fund', '--prefix', '/tmp/pw', `playwright-core@${version}`],
    { stdio: 'inherit' }
  );
  return require('/tmp/pw/node_modules/playwright-core');
}

const { chromium } = loadPlaywright();

const BASE_URL = (process.env.TARSIER_URL || 'http://host.docker.internal:2090').replace(/\/+$/, '');
const USERNAME = process.env.TARSIER_USER || 'admin';
const PASSWORD = process.env.TARSIER_PASSWORD || 'admin';
const OUTPUT_DIR = process.env.OUTPUT_DIR || '/output';

async function wait(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function enforceDarkTheme(page) {
  await page.evaluate(() => {
    try {
      localStorage.setItem('tarsier-theme', 'dark');
    } catch (_) {}
    document.documentElement.setAttribute('data-theme', 'dark');
    if (window.store) {
      window.store.theme = 'dark';
    }
  });
}

async function dismissToast(page) {
  await page.evaluate(() => {
    if (window.store && window.store.toast && typeof window.store.toast.dismiss === 'function') {
      window.store.toast.dismiss();
    }
  });
}

async function waitForCharts(page, selector, timeoutMs = 8000) {
  try {
    await page.waitForSelector(`${selector} canvas`, { timeout: timeoutMs });
    await wait(600);
  } catch (_) {
    await wait(800);
  }
}

async function saveShot(page, filename) {
  fs.mkdirSync(OUTPUT_DIR, { recursive: true });
  const targetPath = path.join(OUTPUT_DIR, filename);
  await enforceDarkTheme(page);
  await dismissToast(page);
  await wait(250);
  await page.screenshot({ path: targetPath, fullPage: false });
  const stats = fs.statSync(targetPath);
  console.log(`[OK] Captured ${filename} (${stats.size} bytes)`);
}

async function main() {
  console.log(`Connecting to Tarsier at ${BASE_URL}`);
  console.log(`Saving screenshots to ${OUTPUT_DIR}`);

  const browser = await chromium.launch({
    headless: true,
    args: ['--no-sandbox', '--disable-setuid-sandbox'],
  });

  const context = await browser.newContext({
    viewport: { width: 1920, height: 1080 },
    deviceScaleFactor: 2,
    colorScheme: 'dark',
  });

  await context.addInitScript(() => {
    try {
      localStorage.setItem('tarsier-theme', 'dark');
    } catch (_) {}
    document.documentElement.setAttribute('data-theme', 'dark');
  });

  const page = await context.newPage();

  try {
    // 1. Login modal before submitting credentials
    await page.goto(BASE_URL, { waitUntil: 'networkidle', timeout: 30000 });
    await page.waitForSelector('dialog.login-dialog[open]', { timeout: 10000 });
    await enforceDarkTheme(page);
    await wait(400);
    await saveShot(page, '01-login.png');

    // Submit login credentials
    await page.fill('#login-username', USERNAME);
    await page.fill('#login-password', PASSWORD);
    await page.click('dialog.login-dialog form button');
    await page.waitForSelector('nav.app-nav', { timeout: 15000 });
    await enforceDarkTheme(page);

    // 2. Overview dashboard (#overview)
    await page.evaluate(() => {
      if (window.store) window.store.setRoute('overview');
    });
    await waitForCharts(page, '#overview-chart');
    await wait(800);
    await saveShot(page, '02-overview-dashboard.png');

    // 3. Host metrics (#host)
    await page.evaluate(() => {
      if (window.store) window.store.setRoute('host');
    });
    await waitForCharts(page, '#chart-cpu');
    await wait(800);
    await saveShot(page, '03-host-metrics.png');

    // 4. Containers list (#containers)
    await page.evaluate(() => {
      if (window.store) window.store.setRoute('containers');
    });
    await page.waitForSelector('section table.striped tbody tr', { timeout: 10000 });
    await wait(800);
    await saveShot(page, '04-containers-list.png');

    // 5. Container detail (#container-detail)
    const containerName = await page.evaluate(() => {
      if (window.containersView && Array.isArray(window.containersView.runningContainers)) {
        const running = window.containersView.runningContainers;
        if (running.length > 0) return running[0].name;
      }
      if (window.containersView && Array.isArray(window.containersView.containers) && window.containersView.containers.length > 0) {
        return window.containersView.containers[0].name;
      }
      return '';
    });

    if (containerName) {
      await page.evaluate((name) => {
        if (window.store) window.store.setRoute('container-detail', { container: name });
      }, containerName);
    } else {
      const detailsBtn = await page.$('a[href^="#container-detail"]');
      if (detailsBtn) {
        await detailsBtn.click();
      }
    }
    await wait(1200);
    await saveShot(page, '05-container-detail.png');

    // 6. Log explorer (#logs) with active source and search query applied
    await page.evaluate(() => {
      if (window.store) window.store.setRoute('logs');
    });
    await wait(1200);

    const logSelection = await page.evaluate(async () => {
      if (!window.logsView) return { ok: false };
      const sources = window.logsView.sources || [];
      // Prefer a running container source with log lines available
      const ordered = sources.slice().sort((a, b) => {
        const aScore = (a.status === 'running' ? 10 : 0) + (a.buffered_lines > 0 ? 5 : 0);
        const bScore = (b.status === 'running' ? 10 : 0) + (b.buffered_lines > 0 ? 5 : 0);
        return bScore - aScore;
      });

      for (const s of ordered) {
        try {
          const res = await fetch('/api/v1/logs?source=' + encodeURIComponent(s.id) + '&limit=50');
          if (!res.ok) continue;
          const data = await res.json();
          if (data && Array.isArray(data.entries) && data.entries.length > 0) {
            window.logsView.selectSource(s);
            // Pick a common search keyword present in one of the entries so matches highlight cleanly
            const sample = data.entries.find((e) => e.line && e.line.length > 10) || data.entries[0];
            const words = (sample.line || '').match(/[A-Za-z]{4,12}/g) || [];
            const preferred = words.find((w) => !['true', 'false', 'null'].includes(w.toLowerCase())) || '';
            return { ok: true, sourceId: s.id, keyword: preferred, count: data.entries.length };
          }
        } catch (_) {}
      }
      return { ok: false };
    });

    await wait(1000);
    if (logSelection.ok && logSelection.keyword) {
      await page.evaluate((kw) => {
        if (window.logsView) {
          window.logsView.query = kw;
          window.logsView.onFilterChange();
        }
      }, logSelection.keyword);
      await wait(800);
    }
    await saveShot(page, '06-log-explorer.png');

    // 7. Log surrounding context modal
    const contextTriggered = await page.evaluate(() => {
      if (!window.logsView) return false;
      if (Array.isArray(window.logsView.entries) && window.logsView.entries.length > 0) {
        const midIdx = Math.min(2, window.logsView.entries.length - 1);
        window.logsView.openContext(window.logsView.entries[midIdx]);
        return true;
      }
      return false;
    });

    if (contextTriggered) {
      await page.waitForSelector('dialog.modal-wide[open]', { timeout: 8000 });
      await wait(900);
    } else {
      await page.evaluate(() => {
        if (window.logsView) {
          const now = Date.now() / 1000;
          window.logsView.contextEntries = [
            { id: 'sample-1', ts: new Date((now - 4) * 1000).toISOString(), ts_src: 1, level: 'info', level_src: 1, stream: 'stdout', line: 'server listening on :8080' },
            { id: 'sample-2', ts: new Date((now - 3) * 1000).toISOString(), ts_src: 1, level: 'warn', level_src: 3, stream: 'stdout', line: 'upstream latency exceeded 250ms threshold' },
            { id: 'sample-3', ts: new Date((now - 2) * 1000).toISOString(), ts_src: 1, level: 'error', level_src: 1, stream: 'stderr', line: 'connection reset by peer while proxying /api/v1/orders' },
            { id: 'sample-4', ts: new Date((now - 1) * 1000).toISOString(), ts_src: 0, level: 'info', level_src: 3, stream: 'stdout', line: 'retrying upstream connection attempt=1' },
          ];
          window.logsView.contextTargetIndex = 2;
          window.logsView.contextLoading = false;
          window.logsView.contextModalOpen = true;
        }
      });
      await wait(500);
    }
    await saveShot(page, '07-log-context-modal.png');

    await page.evaluate(() => {
      if (window.logsView && window.logsView.closeContext) {
        window.logsView.closeContext();
      }
    });

    // 8. Alert rules (#alerts)
    await page.evaluate(() => {
      if (window.store) window.store.setRoute('alerts');
    });
    await wait(1000);
    await saveShot(page, '08-alert-rules.png');

    // 9. Alert rule builder modal
    await page.evaluate(() => {
      if (window.alertsView) {
        if (typeof window.alertsView.openCreateModal === 'function') {
          window.alertsView.openCreateModal();
        } else {
          window.alertsView.modalOpen = true;
        }
      }
    });
    await page.waitForSelector('dialog.rule-builder-dialog[open]', { timeout: 8000 });
    await wait(600);
    await saveShot(page, '09-alert-rule-modal.png');

    await page.evaluate(() => {
      if (window.alertsView && window.alertsView.closeModal) {
        window.alertsView.closeModal();
      }
    });

    // 10. Settings & retention (#settings)
    await page.evaluate(() => {
      if (window.store) window.store.setRoute('settings');
    });
    await wait(1200);
    await saveShot(page, '10-settings-retention.png');

    // 11. Pre-cleanup digest report preview modal
    const reportId = await page.evaluate(() => {
      if (window.settingsView && Array.isArray(window.settingsView.history) && window.settingsView.history.length > 0) {
        const withLogs = window.settingsView.history.find((h) => h.logs_deleted > 0 || h.report_status === 'sent' || h.report_status === 'partial');
        return (withLogs || window.settingsView.history[0]).id;
      }
      return null;
    });

    if (reportId !== null) {
      await page.evaluate((id) => {
        if (window.settingsView && typeof window.settingsView.viewReport === 'function') {
          window.settingsView.viewReport(id);
        }
      }, reportId);
      await page.waitForSelector('dialog.modal-wide[open]', { timeout: 8000 });
      await wait(1000);
    } else {
      await page.evaluate(() => {
        if (window.settingsView) {
          window.settingsView.selectedReport = {
            id: 1,
            trigger: 'manual',
            dry_run: true,
            started_at: new Date().toISOString(),
            report_format: 'text',
            report_status: 'sent',
            report_body: 'Tarsier Pre-Cleanup Digest Report\n=================================\nStatus: DRY RUN\nExpired Logs: 0 lines\nMetrics Pruned: 0 rows\nBytes Reclaimed: 0 B\n',
          };
          window.settingsView.reportLoading = false;
          window.settingsView.reportModalOpen = true;
        }
      });
      await wait(500);
    }
    await saveShot(page, '11-cleanup-report-modal.png');
  } finally {
    await browser.close();
  }
}

main().catch((err) => {
  console.error('Screenshot capture failed:', err);
  process.exit(1);
});
