// 守「线上验收 leg 名单不许跟脚本真身脱钩」这一件事。
//
// 为什么单独立一条：
//   这两条 *_e2e.py 是**真浏览器 + 真服务 + 真模型**的终验，CI 里跑不了
//   （runner 上没有模型、也没有起着的服务），所以它们天然进不了 CI 的 step 清单。
//   而这类文件腐烂的方式是无声的：
//     · 脚本加了 leg（比如 docgen 那条要用 REQUIRE_FILE=1 换前提），runner 不知道
//       → 那条 leg **永远不跑**，而且失败形态是「一切正常」；
//     · runner 里写死文件名，磁盘上脚本改名/新增 → 新脚本永远不跑；
//     · leg 里带了个键（如 PROMPT_KY=docgen），脚本里根本没这个变量
//       → 看起来切了路径，其实两条 leg 跑的是同一条；
//     · leg 的提示词被塞进 runner 的 env 赋值里 → 提示词这一最该被审查的东西
//       藏进 scripts/ 里没人看（而且会被空格切碎）。
//   2026-09-15 复查时这两条脚本的真实处境是**零引用**：ci.yml 没有、preflight.sh
//   没有、docs 没有，只有 /tmp 里几个日志。所以这条守卫盯的是"接线"，不是"跑通"。
//   「跑通」由 scripts/acceptance-live.sh 在真环境里给结论；行为自证（伪造 SKIP 必须
//   判红、伪造 leg 必须真被执行）在 live_e2e_roster_mutation_check.sh 里。
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync, existsSync } from 'node:fs';

const REPO = new URL('../../', import.meta.url);
const read = (p) => readFileSync(new URL(p, REPO), 'utf8');

const RUNNER = 'scripts/acceptance-live.sh';

// 剥掉「整行注释」再查写死清单：本文件允许在注释里点名某个脚本（讲清为什么），
// 但不许在**真执行的代码行**上出现具体文件名。
const stripFullLineComments = (txt) =>
  txt
    .split('\n')
    .filter((l) => !/^\s*#/.test(l))
    .join('\n');

const e2eFiles = () =>
  readdirSync(new URL('web/tests/', REPO))
    .filter((f) => f.endsWith('_e2e.py'))
    .sort();

// 解析 `# LIVE-LEGS: a | b KEY=VAL` —— 与 runner 里的 awk 必须同源同义。
const parseLegs = (txt) => {
  const decl = txt
    .split('\n')
    .filter((l) => /^\s*#\s*LIVE-LEGS:/.test(l))
    .map((l) => l.replace(/^\s*#\s*LIVE-LEGS:\s*/, ''));
  return { lines: decl, legs: (decl[0] ?? '').split('|').map((s) => s.trim()).filter(Boolean) };
};

test('线上验收脚本集合不许为空（枚举失效时这条会先红）', () => {
  const files = e2eFiles();
  assert.ok(
    files.length >= 2,
    `web/tests 下只枚举到 ${files.length} 个 *_e2e.py（期望 ≥2：输入区几何/贴底、流式中间材料）。` +
      `脚本被删光或改名时，下面的枚举断言会空转通过 —— 所以下限也要钉死。 实得：${files.join(', ') || '（空）'}`,
  );
});

test('每个 e2e 脚本必须声明 leg，且 leg 名唯一、写法可被 runner 解析', () => {
  for (const f of e2eFiles()) {
    const { lines, legs } = parseLegs(read(`web/tests/${f}`));
    assert.ok(
      lines.length >= 1,
      `web/tests/${f} 没有声明任何 leg（缺 # LIVE-LEGS: 行）—— runner 枚举到它却不知道跑什么，` +
        `失败形态是「一切正常」`,
    );
    assert.equal(lines.length, 1, `web/tests/${f} 的 # LIVE-LEGS: 声明应恰好 1 行（runner 只读第一行），实际 ${lines.length} 行`);
    assert.ok(legs.length > 0, `web/tests/${f} 的 # LIVE-LEGS: 声明是空的 —— 等于没声明`);

    const names = [];
    for (const leg of legs) {
      const toks = leg.split(/\s+/);
      const name = toks[0];
      assert.ok(name, `web/tests/${f} 有一条 leg 没有名字`);
      assert.match(name, /^[A-Za-z0-9_-]+$/, `web/tests/${f} 的 leg 名「${name}」不合规（只允许字母数字_-）`);
      names.push(name);
      for (const kv of toks.slice(1)) {
        assert.match(
          kv,
          /^[A-Z_][A-Z0-9_]*=[^\s]*$/,
          `web/tests/${f} 的 leg「${name}」里「${kv}」不是无空格的 KEY=VAL —— ` +
            `带空格的赋值会被 runner 的 read -a 切碎；长文本（提示词）必须放进脚本内的预设表`,
        );
      }
    }
    assert.equal(new Set(names).size, names.length, `web/tests/${f} 的 leg 名有重复：${names.join('、')}`);
  }
});

test('leg 里的环境变量必须真的被脚本读（死键 = 切换路径是假的）', () => {
  for (const f of e2eFiles()) {
    const raw = read(`web/tests/${f}`);
    // 查「键是否被读」时必须**把声明行 itself 剥掉**：声明行里就写着 KNOB=1，
    // 拿整份源码去搜，任何键都会命中自己的声明 —— 一条自我满足的断言，
    // 永远绿。（这条洞是 live_e2e_roster_mutation_check.sh 的 A2 抓出来的。）
    const txt = stripFullLineComments(raw).split('\n').filter((l) => l.trim() !== '').join('\n');
    const { legs } = parseLegs(raw);
    for (const leg of legs) {
      const [name, ...kvs] = leg.split(/\s+/);
      for (const kv of kvs) {
        const key = kv.split('=')[0];
        assert.ok(
          new RegExp(`\\b${key}\\b`).test(txt),
          `web/tests/${f} 的 leg「${name}」带了 ${key}=…，但脚本里根本没读 ${key} —— ` +
            `这条 leg 跟默认那条跑的是同一件事，而报告上看起来覆盖了两种路径`,
        );
      }
      // PROMPT_KEY 这种「取预设」的键，值也必须在预设表里，否则脚本会 exit 2。
      const pk = kvs.find((kv) => kv.startsWith('PROMPT_KEY='));
      if (pk) {
        const val = pk.split('=')[1];
        assert.ok(
          new RegExp(`['"]${val}['"]\\s*:`).test(txt),
          `web/tests/${f} 的 leg「${name}」指定 PROMPT_KEY=${val}，但 PROMPT_PRESETS 里没有这个键`,
        );
      }
    }
  }
});

test('runner 必须用通配枚举脚本，禁止写死文件名', () => {
  assert.ok(existsSync(new URL(RUNNER, REPO)), `${RUNNER} 不存在 —— 线上验收又只剩"我手动敲命令"了`);

  const code = stripFullLineComments(read(RUNNER));
  const hard = [...code.matchAll(/\b[a-z0-9_]+_e2e\.py\b/g)].map((m) => m[0]);
  assert.equal(
    hard.length,
    0,
    `${RUNNER} 的执行行里出现了写死的脚本名：${[...new Set(hard)].join('、')}。` +
      `必须用 *_e2e.py 通配枚举 —— 写死的清单漏掉一个，那条 leg 就永远不跑，且没有任何提示`,
  );
  assert.ok(
    /\*_e2e\.py/.test(code),
    `${RUNNER} 里没有 *_e2e.py 通配枚举，脚本集合从哪来？`,
  );
  assert.ok(
    /LIVE-LEGS/.test(code),
    `${RUNNER} 没有读 # LIVE-LEGS: 声明 —— 那跑哪几条 leg 只能靠写死清单，正是上面禁止的`,
  );
});

test('runner 必须把「跑不了」和「没结论」当失败，不许静默变绿', () => {
  const code = stripFullLineComments(read(RUNNER));

  // ① 枚举到 0 个脚本 → 报错（"目录选错/被删光"时会跑 0 个 leg 然后报绿）
  assert.match(code, /没找到任何 \*_e2e\.py/, 'runner 缺少「枚举到 0 个脚本就报错」的守卫');
  // ② 一条 leg 都没跑 → 报错
  assert.match(code, /一条 leg 都没跑/, 'runner 缺少「一条 leg 都没跑就报错」的守卫');
  // ③ SKIP != PASS：默认判失败，要放行必须显式 ALLOW_SKIP=1
  assert.match(code, /ALLOW_SKIP/, 'runner 里没有 ALLOW_SKIP —— SKIP 就没有出口了，或者会被当通过');
  assert.match(code, /SKIP≠PASS/, 'runner 缺少「SKIP ≠ PASS」这个判据本身');
  // ④ 全 SKIP（一个真结论都没有）必须报错
  assert.match(code, /一个真结论都没有/, 'runner 缺少「全部 SKIP 也不能算通过」的守卫');
  // ⑤ 只 exit 0 不打断言小结的 leg 判失败（"没有断言的绿"）
  assert.match(code, /--- \[0-9\]\+\/\[0-9\]\+ ok ---/, 'runner 没有校验断言小结行 —— 没有断言的绿会被放过');
  assert.match(code, /绿得没有断言/, 'runner 缺少「没打断言小结就判失败」的判据');
});
