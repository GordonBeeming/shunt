const fs = require('fs');
const path = require('path');
const { chromium } = require('playwright');

async function main() {
  const html = fs.readFileSync('internal/dashboard/assets/index.html', 'utf8')
    .replace('refresh();\nsetInterval(refresh, 10000);', '');
  const browser = await chromium.launch({ headless: true });
  const page = await browser.newPage({ viewport: { width: 1200, height: 800 } });
  const consoleErrors = [];
  page.on('console', message => {
    if (message.type() === 'error') consoleErrors.push(message.text());
  });
  await page.setContent(html);

  const initial = [{
    name: 'sample', liveSiding: '', routes: [{ key: 'web', kind: 'http', url: 'http://localhost:5000', up: false }],
    sidings: [{ name: 'one', live: false, base: false, phase: 'worktree', runtime: 'missing', runtimeDetail: 'not materialized', serving: false, status: 'starting…', progress: ['first'], guest: 'stopped' }]
  }];
  await page.evaluate(apps => {
    renderApps(apps);
    window.initialProgress = document.querySelector('[role="log"]');
    window.initialProgressEntry = window.initialProgress.firstElementChild;
    window.initialStatus = document.querySelector('.terminal-status');
  }, initial);

  const updated = [{
    name: 'sample', liveSiding: 'one', removal: { siding: 'one', stage: 'guest-removed', age: '12s', resume: 'shunt rm one' },
    routes: [{ key: 'web-live', kind: 'http', url: 'http://localhost:5001', up: true }],
    sidings: [{ name: 'one', live: true, base: true, phase: 'guest', runtime: 'running', runtimeDetail: 'healthy', serving: true, status: 'ready', progress: ['first', 'second'], guest: 'running', ip: '192.0.2.5', dashboard: 'http://192.0.2.5:18888' }]
  }];
  const result = await page.evaluate(apps => {
    renderApps(apps);
    const app = document.querySelector('.app');
    const siding = document.querySelector('.siding');
    const progress = siding.querySelector('[role="log"]');
    return {
      sameProgress: progress === window.initialProgress,
      sameFirstEntry: progress.firstElementChild === window.initialProgressEntry,
      sameStatus: siding.querySelector('.terminal-status') === window.initialStatus,
      progress: [...progress.children].map(node => node.textContent),
      appText: app.textContent,
      routeHref: app.querySelector('.route a')?.getAttribute('href'),
      routeUp: app.querySelector('.route .dot')?.classList.contains('up'),
      live: siding.querySelector('.badge.live')?.textContent,
      base: siding.querySelector('.badge.base')?.textContent,
      phase: siding.querySelector('.badge.phase')?.textContent,
      runtime: siding.querySelector('.runtime')?.textContent,
      detail: siding.querySelector('.runtime-detail')?.textContent,
      dashboard: siding.querySelector('.direct')?.getAttribute('href'),
      fenced: siding.querySelector('.action-fence')?.textContent,
      buttons: siding.querySelectorAll('button').length
    };
  }, updated);

  const expected = {
    sameProgress: true,
    sameFirstEntry: true,
    sameStatus: true,
    progress: ['first', 'second'],
    routeHref: 'http://localhost:5001',
    routeUp: true,
    live: 'live',
    base: 'base',
    phase: 'guest',
    runtime: 'runtime: running · app serving',
    detail: 'healthy',
    dashboard: 'http://192.0.2.5:18888',
    fenced: 'Removal in progress',
    buttons: 0
  };
  for (const [key, value] of Object.entries(expected)) {
    if (JSON.stringify(result[key]) !== JSON.stringify(value)) {
      throw new Error(`${key} = ${JSON.stringify(result[key])}, want ${JSON.stringify(value)}`);
    }
  }
  for (const text of ['live: one', 'stage guest-removed', 'shunt rm one', 'web-live']) {
    if (!result.appText.includes(text)) throw new Error(`updated app text missing ${JSON.stringify(text)}`);
  }

  const colliding = [
    { name: 'a-b', liveSiding: '', routes: [], sidings: [
      { name: 'c', phase: 'worktree', runtime: 'missing', status: 'first-a-b/c', progress: ['a-b/c one'] }
    ] },
    { name: 'a', liveSiding: '', routes: [], sidings: [
      { name: 'b-c', phase: 'worktree', runtime: 'missing', status: 'first-a/b-c', progress: ['a/b-c one'] }
    ] }
  ];
  await page.evaluate(apps => {
    renderApps(apps);
    const byApp = name => [...document.querySelectorAll('.app')].find(node => node.dataset.app === name);
    window.collisionNodes = {
      firstProgress: byApp('a-b').querySelector('[role="log"]'),
      firstStatus: byApp('a-b').querySelector('.terminal-status'),
      secondProgress: byApp('a').querySelector('[role="log"]'),
      secondStatus: byApp('a').querySelector('.terminal-status')
    };
  }, colliding);
  const collisionResult = await page.evaluate(apps => {
    renderApps(apps);
    const byApp = name => [...document.querySelectorAll('.app')].find(node => node.dataset.app === name);
    const firstApp = byApp('a-b'), secondApp = byApp('a');
    const firstSiding = firstApp.querySelector('.siding'), secondSiding = secondApp.querySelector('.siding');
    return {
      order: [...document.querySelectorAll('.app')].map(node => node.dataset.app),
      distinctIDs: firstSiding.id !== secondSiding.id,
      firstProgressIdentity: firstSiding.querySelector('[role="log"]') === window.collisionNodes.firstProgress,
      firstStatusIdentity: firstSiding.querySelector('.terminal-status') === window.collisionNodes.firstStatus,
      secondProgressIdentity: secondSiding.querySelector('[role="log"]') === window.collisionNodes.secondProgress,
      secondStatusIdentity: secondSiding.querySelector('.terminal-status') === window.collisionNodes.secondStatus,
      firstProgress: [...firstSiding.querySelector('[role="log"]').children].map(node => node.textContent),
      secondProgress: [...secondSiding.querySelector('[role="log"]').children].map(node => node.textContent),
      firstStatus: firstSiding.querySelector('.terminal-status').textContent,
      secondStatus: secondSiding.querySelector('.terminal-status').textContent
    };
  }, [
    { name: 'a', liveSiding: '', routes: [], sidings: [
      { name: 'b-c', phase: 'worktree', runtime: 'missing', status: 'second-a/b-c', progress: ['a/b-c one', 'a/b-c two'] }
    ] },
    { name: 'a-b', liveSiding: '', routes: [], sidings: [
      { name: 'c', phase: 'worktree', runtime: 'missing', status: 'second-a-b/c', progress: ['a-b/c one', 'a-b/c two'] }
    ] }
  ]);
  const expectedCollision = {
    order: ['a', 'a-b'],
    distinctIDs: true,
    firstProgressIdentity: true,
    firstStatusIdentity: true,
    secondProgressIdentity: true,
    secondStatusIdentity: true,
    firstProgress: ['a-b/c one', 'a-b/c two'],
    secondProgress: ['a/b-c one', 'a/b-c two'],
    firstStatus: 'second-a-b/c',
    secondStatus: 'second-a/b-c'
  };
  for (const [key, value] of Object.entries(expectedCollision)) {
    if (JSON.stringify(collisionResult[key]) !== JSON.stringify(value)) {
      throw new Error(`collision ${key} = ${JSON.stringify(collisionResult[key])}, want ${JSON.stringify(value)}`);
    }
  }

  const screenshotDir = process.env.SHUNT_DASHBOARD_SCREENSHOT_DIR;
  for (const viewport of [{ width: 1920, height: 1080 }, { width: 390, height: 844 }, { width: 320, height: 720 }]) {
    await page.setViewportSize(viewport);
    const overflow = await page.evaluate(() => ({
      document: document.documentElement.scrollWidth - document.documentElement.clientWidth,
      body: document.body.scrollWidth - document.body.clientWidth
    }));
    if (overflow.document > 0 || overflow.body > 0) {
      throw new Error(`${viewport.width}px horizontal overflow: ${JSON.stringify(overflow)}`);
    }
    if (screenshotDir) {
      fs.mkdirSync(screenshotDir, { recursive: true });
      await page.screenshot({ path: path.join(screenshotDir, `dashboard-${viewport.width}.png`), fullPage: true });
    }
  }

  const afterRemoval = structuredClone(updated);
  delete afterRemoval[0].removal;
  afterRemoval[0].sidings[0].status = '';
  await page.evaluate(apps => renderApps(apps), afterRemoval);
  const unfenced = await page.locator('.siding .actions').textContent();
  if (!unfenced.includes('Stop') || unfenced.includes('Removal in progress')) {
    throw new Error(`actions were not restored after removal cleared: ${JSON.stringify(unfenced)}`);
  }
  const releasedResult = await page.evaluate(apps => {
    renderApps(apps);
    const siding = document.querySelector('.app[data-app="released-app"] .siding');
    return {
      releasedBadge: siding.querySelector('.badge.released')?.textContent,
      liveBadge: siding.querySelector('.badge.live')?.textContent
    };
  }, [{
    name: 'released-app', liveSiding: 'one', frontDoorReleased: true, routes: [],
    sidings: [{ name: 'one', live: true, released: true, base: false, phase: 'guest', runtime: 'running', serving: false, status: '', progress: [], guest: 'running' }]
  }]);
  if (releasedResult.releasedBadge !== 'released' || releasedResult.liveBadge) {
    throw new Error(`released badge = ${JSON.stringify(releasedResult)}, want a released badge and no live badge`);
  }

  if (consoleErrors.length) throw new Error(`console errors: ${consoleErrors.join('; ')}`);
  await browser.close();
  console.log('dashboard DOM reconciliation: ok');
}

main().catch(error => {
  console.error(error);
  process.exitCode = 1;
});
