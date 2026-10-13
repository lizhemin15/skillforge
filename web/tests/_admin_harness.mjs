// 从 admin.js 源码里抽真函数/真对象常量的共用工具。
//
// 为什么单独成文件：skill_template_files.test.mjs 里这套抽法踩过两个坑
// （HTML 实体里的分号把函数切断、`=> {` 被当成表达式体），修好后如果每个
// 新测试各抄一份，第二个抄的人会连着旧坑一起抄走。
//
// 命名以 `_` 开头且不以 `.test.mjs` 结尾 —— preflight 是按 `web/tests/*.test.mjs`
// 枚举的，这个文件不会被当测试跑（它自己也没有断言）。

let failures = 0;

export function check(name, cond, extra = '') {
  if (cond) {
    console.log(`  ok   ${name}`);
  } else {
    failures++;
    console.log(`  FAIL ${name}${extra ? ' — ' + extra : ''}`);
  }
}

export function failCount() {
  return failures;
}

// 抽出真函数源码。admin.js 里有三种写法，都得认（只认一种会「找不到函数」而误判成删了）：
//   function foo(...) {...}            —— 函数声明
//   const foo = (...) => ...;          —— 箭头（表达式体）
//   window.foo = async (...) => {...}  —— 挂 window 的箭头（函数体）
export function extractFunction(source, name) {
  const patterns = [`function ${name}(`, `const ${name} = `, `window.${name} = `];
  const hit = patterns.map((p) => source.indexOf(p)).filter((i) => i >= 0).sort((a, b) => a - b)[0];
  if (hit === undefined) throw new Error(`在 admin.js 里找不到 ${name} —— 被删/改名了？`);
  const declStart = hit;
  const arrowEnd = source.indexOf('=>', declStart);
  const braceStart = source.indexOf('{', declStart);
  // 表达式体箭头 = `=>` 之后（跳过空白）不是 `{`。
  // 别用「=> 出现在 { 之前」当判据：`async (x) => {` 也满足它，只会抽到函数头那半行。
  const afterArrow = arrowEnd < 0 ? '' : source.slice(arrowEnd + 2).replace(/^\s+/, '')[0];
  const isExpressionArrow =
    arrowEnd >= 0 && afterArrow !== '{' && arrowEnd < (braceStart < 0 ? Infinity : braceStart);
  if (isExpressionArrow) {
    // 取到本行行尾。不能用「第一个分号」——HTML 实体内就带分号，会把函数从中间切断。
    const lineEnd = source.indexOf('\n', arrowEnd);
    const line = source.slice(declStart, lineEnd < 0 ? source.length : lineEnd).trimEnd();
    return line.endsWith(';') ? line : line + ';';
  }
  let depth = 0;
  for (let i = braceStart; i < source.length; i++) {
    if (source[i] === '{') depth++;
    else if (source[i] === '}') {
      depth--;
      if (depth === 0) return source.slice(declStart, i + 1);
    }
  }
  throw new Error(`${name} 花括号不配平`);
}

// 抽出 `const <name> = { ... }`（对象字面量，配平花括号）。
export function extractObjectConst(source, name) {
  const start = source.indexOf(`const ${name} = {`);
  if (start < 0) throw new Error(`在 admin.js 里找不到 const ${name} = { —— 被删/改名了？`);
  let depth = 0;
  for (let i = source.indexOf('{', start); i < source.length; i++) {
    if (source[i] === '{') depth++;
    else if (source[i] === '}') {
      depth--;
      if (depth === 0) return source.slice(start, i + 1) + ';';
    }
  }
  throw new Error(`const ${name} 花括号不配平`);
}
