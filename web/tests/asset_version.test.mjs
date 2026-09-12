// 缓存版本号一致性守卫 —— 目标：让"改了 JS/CSS 却没 bump ?v="不可能悄悄上线。
//
// 这个项目栽过两次：代码真修好了，但老浏览器加载的还是旧文件，
// 用户看到的现象就是"你没修"。根因是 ?v= 只是个人工字符串，
// 改了文件忘了改它，没有任何东西会报错。
//
// 三道断言：
//   1. HTML 里出现的每个 ?v= 资源都必须在 manifest 里登记（新资源被顺手加进 HTML 时会红）
//   2. HTML 的 ?v= 必须与 manifest 记录一致（手改一处、漏另一处时会红）
//   3. manifest 记录的 sha256 必须等于文件当前内容（改了文件没 bump 时会红）
import { readFileSync } from 'node:fs';
import { createHash } from 'node:crypto';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));
const WEB = join(HERE, '..');

const HTMLS = ['index.html', 'admin.html'];
const REF = /["'(]([^"'()\s]*?\.(?:js|css))\?v=([0-9A-Za-z]+)["')]/g;

const manifest = JSON.parse(readFileSync(join(WEB, 'assets.manifest.json'), 'utf8')).assets;

let checks = 0, failures = 0;
function check(name, ok, detail) {
  checks++;
  if (ok) { console.log(`  ok   ${name}`); return; }
  failures++;
  console.log(`  FAIL ${name}${detail ? ' — ' + detail : ''}`);
}

function diskPath(url) {
  return url.startsWith('/assets/') ? join(WEB, url.slice('/assets/'.length)) : join(WEB, url.replace(/^\//, ''));
}
function hashFile(p) {
  return createHash('sha256').update(readFileSync(p)).digest('hex');
}

console.log('缓存版本号 · HTML ↔ manifest ↔ 文件内容');

const seen = new Map(); // url -> {v, files:Set}
for (const name of HTMLS) {
  const html = readFileSync(join(WEB, name), 'utf8');
  for (const m of html.matchAll(REF)) {
    const [, url, v] = m;
    if (!seen.has(url)) seen.set(url, { v, files: new Set() });
    const rec = seen.get(url);
    rec.files.add(name);
    check(`${name} 里 ${url} 的 ?v= 与另一处一致`, rec.v === v, `${rec.v} vs ${v}`);
  }
}

const registered = new Set(Object.keys(manifest));
for (const [url, { v, files }] of seen) {
  check(`${url} 已登记进 manifest`, registered.has(url),
    `新资源没登记 → 跑 node web/tests/gen-asset-manifest.mjs`);
  if (registered.has(url)) {
    check(`${url} 的 ?v= 与 manifest 一致`, manifest[url].v === v,
      `HTML=${v} manifest=${manifest[url].v}（改了一处漏了另一处）`);
  }
}
for (const url of registered) {
  check(`${url} 仍被某个 HTML 引用`, seen.has(url), 'manifest 里留着已下线的资源');
}

// 第三道，也是最要紧的一道：内容动过就必须换 ?v=。
let drifted = 0;
for (const [url, rec] of Object.entries(manifest)) {
  let actual;
  try { actual = hashFile(diskPath(url)); } catch (e) {
    check(`${url} 文件存在`, false, String(e.message)); continue;
  }
  const same = actual === rec.sha256;
  check(`${url} 内容未被改动（?v=${rec.v}）`, same);
  if (!same) drifted++;
}
if (drifted) {
  console.log(`\n  ${drifted} 个资源内容变了但 ?v= 没换 —— 老浏览器会继续用旧文件。`);
  console.log('  修法：bump 掉两个 HTML 里的 ?v=，再跑 node web/tests/gen-asset-manifest.mjs');
}

console.log(`\n${checks} 项断言`);
if (failures) { console.log(`${failures} 项失败`); process.exit(1); }
console.log('全部通过');
