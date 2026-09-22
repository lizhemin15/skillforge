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

test('多 leg 脚本的 leg 之间必须有区分项（两条腿跑同一件事 = 假对照）', () => {
  // 为什么单独立这条（而不是靠上面那条「env 必须被脚本读」兜住）：
  // 2026-09-22 线上实测踩到 —— chat_doubts_e2e.py 声明过
  //   `# LIVE-LEGS: doubts-informed TIMEOUT_S=300 | doubts-vague TIMEOUT_S=300`
  // 两条腿的 env 只差 TIMEOUT_S，脚本里 LEG 又有默认值 'informed'，于是「vague」那条腿
  // 实际跑的是 informed 的原话 + informed 的判据，回来报 6/6 绿。上面那条断言管不着它：
  // 每个键（TIMEOUT_S）确实被脚本读了，键和值都不是死的 —— 死的是「两条腿没有区别」。
  // 假对照比没对照更毒：报告上看着覆盖了两种路径，实际只跑了一条，而另一条永远绿。
  for (const f of e2eFiles()) {
    const { legs } = parseLegs(read(`web/tests/${f}`));
    if (legs.length < 2) continue;
    const sigs = legs.map((leg) => {
      const [name, ...kvs] = leg.split(/\s+/);
      // TIMEOUT_S 不算区分项：它只决定「等多久才砍」，不改变脚本做什么。
      // 「300 vs 600」这种差别套上「两条腿都覆盖了」的皮，实际跑的是同一件事。
      return { name, diff: kvs.filter((kv) => !kv.startsWith('TIMEOUT_S=')).sort().join(' ') };
    });
    const seen = new Map();
    for (const s of sigs) {
      if (seen.has(s.diff)) {
        throw new Error(
          `web/tests/${f} 的 leg「${s.name}」和「${seen.get(s.diff)}」的入参完全一样` +
            `（都不带任何非 TIMEOUT_S 的键）—— 这两条腿跑的是同一件事，` +
            `报告上却会显示覆盖了两条路径（假对照）。给每条腿一个真正区分行为的键，` +
            `或者把多余那条删掉。`,
        );
      }
      seen.set(s.diff, s.name);
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

// 小结格式必须统一：runner 用 `grep -oE '--- [0-9]+/[0-9]+ ok ---'` 抠断言数，
// 抠不到就判「绿得没有断言」→ FAIL。
// 2026-09-17 线上实测（这不是假想）：admin_train_progress 腿原本打的是
// `--- 9 ok / 0 fail ---`，9 条断言全绿，整条 leg 却被判红 ——
// **尺子格式不匹配被当成了被测系统红**。这条测试就是不让它再发生：
// 谁以后写新 leg 用了别的格式，在 CI 里当场红，而不是等线上验收时才让人一头雾水。
test('每条 leg 都必须打 runner 认得出的 `--- N/M ok ---` 小结（否则绿也会被判红）', () => {
  const files = e2eFiles();
  assert.ok(files.length > 0, '没有枚举到任何 _e2e.py —— 上面那条测试应该先红了');
  // 只看**真打印语句**：注释/文档里讲历史格式是允许的（本文件上面那段就在讲），
  // 但那不算「这条腿会打对格式」。首跑就是被我自己写在注释里的 `9 ok / 0 fail ---`
  // 例子命中的 —— 尺子先抓自己，属于好尺子的正常表现，按语义收窄即可，别改断言迁就。
  const printLines = (txt) =>
    txt
      .split('\n')
      .filter((l) => !/^\s*#/.test(l) && /print\(/.test(l))
      .join('\n');
  for (const f of files) {
    const txt = printLines(read(new URL(`web/tests/${f}`, REPO)));
    assert.ok(
      txt.length > 0,
      `web/tests/${f} 里一行打印都没有 —— 那它靠什么给 runner 报断言数？`
    );
    assert.ok(
      /---[^'"`\n]*ok ---/.test(txt),
      `web/tests/${f} 没有打 \`--- N/M ok ---\` 小结：runner 抠不到断言数，` +
        '这条腿绿了也会被判红（历史形态：`--- 9 ok / 0 fail ---`）'
    );
    assert.ok(
      !/[^/\n]*\bok \/ [^/\n]*fail ---/.test(txt),
      `web/tests/${f} 打的是旧格式 \`N ok / M fail ---\`（历史上就这么假红过），` +
        '请统一成 `--- N/M ok ---`'
    );
  }
});
