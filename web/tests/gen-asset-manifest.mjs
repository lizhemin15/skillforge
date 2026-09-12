#!/usr/bin/env node
// 生成 / 刷新 web/assets.manifest.json。
//
// 存在的理由：这个项目已经两次栽在"改了 JS/CSS 但没 bump ?v="上 ——
// 代码明明修好了，老浏览器里加载的还是旧文件，用户看到的是"没修"。
// manifest 记下"某个哈希的内容曾经挂在某个 ?v= 下"，
// 于是 asset_version.test.mjs 能自动发现漂移。
//
// 改了前端资源后：bump 掉 HTML 里的 ?v=（两个 HTML 都要），然后
//     node web/tests/gen-asset-manifest.mjs
// 再跑测试确认。
import { readFileSync, writeFileSync } from 'node:fs';
import { createHash } from 'node:crypto';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const HERE = dirname(fileURLToPath(import.meta.url));
const WEB = join(HERE, '..');        // web/
const ROOT = join(WEB, '..');        // 仓库根

const HTMLS = ['index.html', 'admin.html'];
const REF = /["'(]([^"'()\s]*?\.(?:js|css))\?v=([0-9A-Za-z]+)["')]/g;

export function diskPath(url) {
  // /assets/js/chat.js?v=X → web/js/chat.js
  if (url.startsWith('/assets/')) return join(WEB, url.slice('/assets/'.length));
  return join(WEB, url.replace(/^\//, ''));
}

export function hashFile(path) {
  return createHash('sha256').update(readFileSync(path)).digest('hex');
}

// 扫两个 HTML，收集 URL → ?v=。
export function scanHtml() {
  const found = new Map();
  for (const name of HTMLS) {
    const html = readFileSync(join(WEB, name), 'utf8');
    for (const m of html.matchAll(REF)) {
      const [, url, v] = m;
      if (!found.has(url)) found.set(url, { v, in: [] });
      const rec = found.get(url);
      rec.in.push(name);
      if (rec.v !== v) {
        throw new Error(`${url} 在两个 HTML 里的 ?v= 不一致：${rec.v} vs ${v}`);
      }
    }
  }
  return found;
}

function main() {
  const found = scanHtml();
  const out = { _comment: '由 web/tests/gen-asset-manifest.mjs 生成；勿手改，改了会被 asset_version 测试抓住', assets: {} };
  for (const [url, { v, in: where }] of [...found].sort()) {
    out.assets[url] = { v, sha256: hashFile(diskPath(url)), in: where };
  }
  const target = join(WEB, 'assets.manifest.json');
  writeFileSync(target, JSON.stringify(out, null, 2) + '\n');
  console.log(`写入 ${target}（${Object.keys(out.assets).length} 个资源）`);
  for (const [url, { v }] of [...found].sort()) console.log(`  ${v}  ${url}`);
}

if (process.argv[1] === fileURLToPath(import.meta.url)) main();
