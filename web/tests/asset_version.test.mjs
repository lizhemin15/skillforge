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
import { execFileSync } from 'node:child_process';
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

// 第四道：问 git「这个资源跟上一个提交比，内容变了没有」。
//
// 为什么前三道不够：三道全是「HTML ↔ manifest ↔ 磁盘」三者互相比，
// 而 gen-asset-manifest.mjs 会把当前哈希照抄进 manifest、v 也是照抄 HTML 的。
// 也就是说——改完文件**照着文档重新生成一次 manifest**，前三道就会全绿，
// 哪怕 ?v= 一个字没动。这个脚本存在的唯一理由（「改了文件忘了 bump」）恰好漏出去了。
// 假断言长什么样，这条就是标本：断言自己喂自己，永远绿。
//
// 唯一能判定「变没变」的外部事实是 git 历史。CI 上 HEAD~1 就是上一次提交，
// 所以 checkout 必须 fetch-depth: 2（浅克隆只有 1 个提交，这条会跳过并明说）。
const urlToRepoPath = (url) => 'web/' + (url.startsWith('/assets/') ? url.slice('/assets/'.length) : url.replace(/^\//, ''));
// 取 Buffer 而不是字符串：前端资源里有压缩过的单行 JS/CSS，按字符串比会因编码/换行
// 归一化出现「看起来一样」，按字节比才是「内容真的一样」。
//
// ⚠️ catch 只吞「git 以非 0 退出」这一种。第一版写的是裸 catch，结果把
// 「ROOT 这个变量根本没定义」的 ReferenceError 也吞了 —— 表现形式是这条断言
// 一路打印「skip（浅克隆？）」，看着像环境限制，其实是断言自己写坏了。
// 太宽的 catch 是假绿的常见来源：它让「代码坏了」长得跟「环境不支持」一模一样。
const ROOT = join(WEB, '..');
const git = (...args) => {
  try {
    return { ok: true, out: execFileSync('git', args, { cwd: ROOT, maxBuffer: 32 << 20 }) };
  } catch (e) {
    if (typeof e.status !== 'number') throw e;   // 不是 git 的退出码 = 脚本自身出错，直接炸出来
    return { ok: false, out: Buffer.alloc(0) };
  }
};
const hasPrev = git('rev-parse', '--verify', '--quiet', 'HEAD~1').ok;
if (!hasPrev) {
  console.log('  skip 与上一提交比对（拿不到 HEAD~1：浅克隆？给 checkout 加 fetch-depth: 2）');
} else {
  const prevManifestRaw = git('show', 'HEAD~1:web/assets.manifest.json');
  if (!prevManifestRaw.ok || prevManifestRaw.out.length === 0) {
    console.log('  skip 与上一提交比对（HEAD~1 里还没有 manifest）');
  } else {
    const prev = JSON.parse(prevManifestRaw.out.toString('utf8')).assets || {};
    let compared = 0;
    for (const [url, rec] of Object.entries(manifest)) {
      const prevFile = git('show', `HEAD~1:${urlToRepoPath(url)}`);
      if (!prevFile.ok) continue;                       // 资源是这次新加的，没有「上个版本」可比
      compared++;
      const changed = Buffer.compare(prevFile.out, readFileSync(diskPath(url))) !== 0;
      const bumped = !prev[url] || prev[url].v !== rec.v;
      check(`${url} 内容变了就换 ?v=（git 比对）`, !changed || bumped,
        `内容与上一提交不同，但 ?v= 还是 ${rec.v} —— 老浏览器会继续用旧文件`);
    }
    console.log(`  （git 比对覆盖 ${compared} 个资源）`);
  }
}

console.log(`\n${checks} 项断言`);
if (failures) { console.log(`${failures} 项失败`); process.exit(1); }
console.log('全部通过');
