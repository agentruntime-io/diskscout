// Renders banner.html to banner.png (README) and social-preview.png (GitHub social preview) with headless Edge or Chrome.
// usage: node render.js [path to msedge.exe or chrome.exe]
const { spawn, execSync } = require('child_process');
const fs = require('fs'), os = require('os'), path = require('path');

const browser = process.argv[2] || ['C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe', 'C:/Program Files/Microsoft/Edge/Application/msedge.exe',
  'C:/Program Files/Google/Chrome/Application/chrome.exe'].find(p => fs.existsSync(p));
if (!browser) { console.error('no Edge or Chrome found: pass the path as the first argument'); process.exit(1); }
const page = 'file:///' + path.resolve(__dirname, 'banner.html').replace(/\\/g, '/');
const jobs = [{ hash: 'banner', out: 'banner.png', w: 1600, h: 400, dpr: 1.5 }, { hash: 'social', out: 'social-preview.png', w: 1280, h: 640, dpr: 1 }];
const sleep = ms => new Promise(r => setTimeout(r, ms));

(async () => {
  const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'ds-banner-'));
  const proc = spawn(browser, ['--headless=new', '--disable-gpu', '--hide-scrollbars', '--remote-debugging-port=9334', `--user-data-dir=${profile}`, '--no-first-run', 'about:blank'], { stdio: 'ignore' });
  try {
    let targets = [];
    for (let i = 0; i < 60 && !targets.length; i++) { try { targets = (await (await fetch('http://127.0.0.1:9334/json/list')).json()).filter(t => t.type === 'page'); } catch { } await sleep(250); }
    if (!targets.length) throw new Error('no browser page to drive');
    const ws = new WebSocket(targets[0].webSocketDebuggerUrl);
    await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
    let id = 0; const pending = new Map();
    ws.onmessage = e => { const m = JSON.parse(e.data); if (m.id && pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id); } };
    const send = (method, params = {}) => new Promise(res => { const i = ++id; pending.set(i, res); ws.send(JSON.stringify({ id: i, method, params })); });
    await send('Page.enable');
    for (const j of jobs) {
      await send('Emulation.setDeviceMetricsOverride', { width: j.w, height: j.h, deviceScaleFactor: j.dpr, mobile: false });
      await send('Page.navigate', { url: `${page}#${j.hash}` });
      await sleep(1200);
      await send('Runtime.evaluate', { expression: 'location.reload()' }); // the layout is chosen from the hash, so load it fresh
      await sleep(1500);
      const shot = await send('Page.captureScreenshot', { format: 'png', clip: { x: 0, y: 0, width: j.w, height: j.h, scale: 1 } });
      const file = path.resolve(__dirname, j.out);
      fs.writeFileSync(file, Buffer.from(shot.result.data, 'base64'));
      console.log('wrote', j.out, `${j.w * j.dpr}x${j.h * j.dpr}`, Math.round(fs.statSync(file).size / 1024) + ' KB');
    }
    ws.close();
  } finally {
    try { execSync(`taskkill /PID ${proc.pid} /T /F`, { stdio: 'ignore' }); } catch { }
    await sleep(500);
    try { fs.rmSync(profile, { recursive: true, force: true }); } catch { }
  }
})().catch(e => { console.error('failed:', e.message); process.exit(1); });
