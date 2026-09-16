// 守「本地 preflight 与 CI 是同一道闸门」这一件事。
//
// 为什么单独立一条：这两份闸门写在两个文件里、两套语法里（shell 脚本 / GitHub Actions YAML），
// 它们分叉时的表现是**最坏的那种**——本地全绿、推送后 CI 红，而人要等 runner 排队才发现。
// 2026-09-13 连踩两次，都是这个形状：
//   ① preflight 的前身只跑裸 `go test ./...`，CI 跑的是 `go test -race -count=1 ./...`
//      → 本地漏掉一个真数据竞争（internal/skillgen 的测试闭包），推送后白红一次；
//   ② CI 的前端 step 是写死的 9 个文件名，磁盘上有 11 个测试
//      → skill_file_tree_edit_button 和 suggest_timeout_budget **从来没在 CI 里跑过**。
//      「漏加 step」等于「这个测试不存在」，而且是无声的。
// 所以这里把「枚举」和「参数一致」都钉死：谁把清单写回去、谁改了一边参数，立刻红。
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync, existsSync } from 'node:fs';

const REPO = new URL('../../', import.meta.url);
const read = (p) => readFileSync(new URL(p, REPO), 'utf8');

const CI = read('.github/workflows/ci.yml');
const PF = read('scripts/preflight.sh');

// 枚举仓库里所有「注入自证」脚本。它们的共同点是：往出货文件里注入真实故障、
// 要求对应断言变红、再显式还原。这类脚本有个致命性质 —— **不跑就没价值**，
// 而且不跑的时候它是静的（不会报错、不会腐烂到崩，只是永远绿）。
// 实测代价：scripts/category_guard_inject.py 的一条锚点跟实现脱钩很久，
// 脚本每次都在打印「锚点失效 ✗」，但它既不在 CI 也没人手动跑，谁都没看见。
//
// 2026-09-15 补：原来只枚举 web/tests 下的 `*_mutation_check.sh`，于是那两个
// `*_mutation_check.py`（chat_composer / chat_trace）落在名单外 —— 后果实测到了：
// chat_trace_mutation_check.py 只在 ci.yml 里跑，scripts/preflight.sh 里没有，
// 本地闸门比 CI 少一道，本地全绿推上去才红。后缀不是防线，能不能被漏掉才是。
//
// 2026-09-16 补：`deploy/offline/tests/` 是第二个同名黑洞 —— 那里的 .sh 既不叫
// `_mutation_check` 也不在 web/tests 下，于是**整个目录自动免疫**：新加的
// install_probe_test.sh（装后服务探测的正向 + 两路注入自证）写好了、能跑了，
// 却不会有任何机制要求 CI/preflight 调用它。所以这里按**目录**收（该目录下每个
// .sh 都必须被两边调用），而不是再加一个后缀规则。目录比后缀更难绕过：
// 往后往那儿放测试的人不需要记得改这个文件。
//
// 2026-09-16 再补：按目录收只收了 `.sh`，同目录的 `selftest_parse_live_check.py`
// （`-selftest` 的「文档解析服务」栏真跑）照样漏 —— 它写死的检查项数量在 F3/F5
// 加了两栏之后失配，7 个场景全红，而它没被任何闸门调用，所以没人看见，
// 报的还是一句误导人的「输出格式变了」。**「按目录收」必须连扩展名一起收干净**，
// 否则就是换了个姿势的黑洞。`.py` 里只有真跑型的尺子算，被 spawn 的假服务 helper 除外。
// 2026-09-16 三补（由自己那把负向自证尺子抓出来的真缺陷）：
// 上面这些枚举**按组拼成一个大数组**，配一句全局下限 `scripts.length >= 8`。
// 于是「某一个目录的 readdir 整条死掉」是**看不见**的：另外几个目录照样把总数
// 顶过下限，测试全绿 —— 而后果恰恰是本文件存在的理由（整个目录再次免疫）。
// 这不是假设：把 deploy/offline/tests 那一组的 filter 改成恒 false，
// 老写法下守卫 0 报错，`preflight_parity_mutation_check.sh` 的 A3 直接抓到了。
// 所以改成**按组枚举 + 每组各自非空 + 点名**：任何一组塌成 0 都要红，
// 而且要报出是哪一组（报「总数不够」是没法归因的）。
const PY_HELPERS = new Set(['fake_http.py']);
// deploy/ocr 里不是「尺子」的 .py：ocrd.py 是被测的生产件（要求它「被调用」没意义），
// make_fixture.py 是被测试 spawn 的造料 helper。白名单以外的 .py 一律要求两边闸门真调用。
const OCR_NON_TEST = new Set(['ocrd.py', 'make_fixture.py']);
const selfCheckGroups = () => [
  ['internal/api 的 *_mutation_check.sh',
    readdirSync(new URL('internal/api/', REPO)).filter((f) => f.endsWith('_mutation_check.sh')).map((f) => `internal/api/${f}`)],
  ['web/tests 的 *_mutation_check.sh',
    readdirSync(new URL('web/tests/', REPO)).filter((f) => f.endsWith('_mutation_check.sh')).map((f) => `web/tests/${f}`)],
  ['web/tests 的 *_mutation_check.py',
    readdirSync(new URL('web/tests/', REPO)).filter((f) => f.endsWith('_mutation_check.py')).map((f) => `web/tests/${f}`)],
  ['scripts 的 inject*.py',
    readdirSync(new URL('scripts/', REPO)).filter((f) => /inject.*\.py$/.test(f)).map((f) => `scripts/${f}`)],
  ['deploy/ocr 的可跑单测（.py 除生产件与造料 helper）',
    // 2026-09-17 补：这是**第三个免疫区**，形状和前两个一模一样。
    // deploy/ocr/test_ocrd_quality.py 是「可选中页直取 / 乱码页回落 OCR」这条
    // 用户抱怨的自证尺子，写好、能在本机跑，但 0 引用 —— 于是它静静地烂了：
    // 钉死的版本串还是 `ocrd-v5-quality`，而真值早改成了 `ocrd-v5-runtime-guard`
    // （改名的那次提交只改了实现和部署门禁，没人被提醒去改这把尺子）。
    // 修法不许再抄字面量，改成**与部署门禁对账**（见该文件
    // test_版本串必须与部署门禁一致）—— 抄的那份永远不会红。
    //
    // 为什么是「整目录减白名单」而不是「只收 test_*.py」：后者是又一个后缀黑洞，
    // 本文件上面已经为这个形状付过一次代价。第一次写这条时就踩了 —— C2 自证
    // （往 deploy/ocr 丢一个 `zzz_stray_probe_check.py`）当场判「注入后守卫仍全绿」，
    // 因为那个探针不以 test_ 开头。任何**新名字**的尺子都会重蹈覆辙，所以按目录收。
    // 排掉的两类：ocrd.py 是生产件（被测对象，不能要求「被调用」）；
    // make_fixture.py 是被测试 spawn 的造料 helper。剩下 verify_*.sh 要 PyInstaller
    // 冻结二进制（本机没有就 SKIP/FAIL exit 2，见 verify_runtime_loss.sh:56），
    // 它们不在本组，由 deploy/ocr/Dockerfile:97 与 scripts/deploy_ocrd.sh 在发版时挡住。
    readdirSync(new URL('deploy/ocr/', REPO))
      .filter((f) => f.endsWith('.py') && !OCR_NON_TEST.has(f))
      .map((f) => `deploy/ocr/${f}`)],
  ['deploy/offline/tests 的可执行测试（.sh 全收 + .py 除 helper）',
    readdirSync(new URL('deploy/offline/tests/', REPO))
      .filter((f) => f.endsWith('.sh') || (f.endsWith('.py') && !PY_HELPERS.has(f)))
      .map((f) => `deploy/offline/tests/${f}`)],
];
const selfCheckScripts = () => selfCheckGroups().flatMap(([, files]) => files);

// 注入模式也必须被真调用：只跑正向模式的 CI 里，INJECT 那两路会静静地腐烂
// （它们只在被显式传 INJECT= 时才生效，平时连语法错都不会暴露）。
// 这类「可选参数才是本体」的自证脚本，光看「文件被调用了」不够。
const INJECT_MODES = ['INJECT=1', 'INJECT=2'];

test('preflight 与 CI 的 Go 单测命令必须逐字一致（含 -race -count=1）', () => {
  const WANT = 'go test -race -count=1 ./...';

  // 从 CI 的 Unit tests step 里取**真正执行**的那一行。
  const mCi = CI.match(/name:\s*Unit tests\s*\n\s*run:\s*(go test[^\n]*)/);
  assert.ok(mCi, 'ci.yml 里找不到「Unit tests」step 的 run 行 —— step 被改名/改写了，这条守卫失效');
  const ciCmd = mCi[1].trim();

  // 取 preflight 里真正执行的那一行（`if go test ...`）。
  //
  // **不许用 `PF.includes(WANT)`**：这串字在 preflight 里还出现在 `step '5/6 go test ...'`
  // 这个标签里，裸子串检索会命中标签、于是把「命令被改坏」判成绿的 ——
  // 实测注入 `去掉 -race` 后 includes 仍然为真，假阴性。必须锚定到行首的命令形态：
  // 注释行（# 开头）和 step 标签行（step ' 开头）都不会被这条正则捞进来。
  const pfCmds = [...PF.matchAll(/^\s*(?:if\s+)?(go test [^\n>]*)/gm)].map((m) => m[1].trim());
  assert.equal(
    pfCmds.length,
    1,
    `preflight.sh 里执行形态的 go test 命令应恰好 1 条，实际 ${pfCmds.length} 条：${pfCmds.join(' | ')}`,
  );
  const pfCmd = pfCmds[0];

  assert.equal(
    pfCmd,
    WANT,
    `preflight 跑的是「${pfCmd}」，CI 跑的是「${ciCmd}」—— 两边分叉了。` +
      `去掉 -race 会漏掉数据竞争，去掉 -count=1 会让缓存掩盖测试间污染；` +
      `两者都是本地测不出、推送后才红的类型。`,
  );
  assert.equal(
    ciCmd,
    WANT,
    `ci.yml 的单测命令变成了「${ciCmd}」：-race 漏数据竞争、-count=1 关缓存防止测试互相污染，` +
      `这两样去掉都是"本地绿、CI 绿、上线炸"。要改就两边一起改。`,
  );
});

test('前端回归必须枚举目录，禁止写死文件清单', () => {
  // 写死的形态长这样：`node web/tests/xxx.test.mjs`
  const hardcoded = /node\s+web\/tests\/[\w.-]+\.test\.mjs/g;

  const inCi = [...CI.matchAll(hardcoded)].map((m) => m[0]);
  assert.equal(
    inCi.length,
    0,
    `ci.yml 里又出现了写死的测试清单：${inCi.join('、')}。` +
      `写死过 9 个 step、磁盘上却有 11 个测试文件，漏掉的那两个从来没跑过 —— ` +
      `新加测试时必须用 web/tests/*.test.mjs 枚举，别加单条 step。`,
  );

  const inPf = [...PF.matchAll(hardcoded)].map((m) => m[0]);
  assert.equal(
    inPf.length,
    0,
    `scripts/preflight.sh 里出现了写死的测试清单：${inPf.join('、')}。改用枚举。`,
  );

  for (const [name, txt] of [['ci.yml', CI], ['scripts/preflight.sh', PF]]) {
    assert.ok(
      txt.includes('web/tests/*.test.mjs'),
      `${name} 里没有 web/tests/*.test.mjs 枚举 —— 新加的前端回归文件不会被跑。`,
    );
  }
});

test('枚举为空时必须报错，不许静默通过', () => {
  // 枚举的代价是「目录选错 / 文件被删光」时会跑 0 个测试且一切"正常"。
  // 这两种情况都必须显式报错，否则闸门会以"全绿"的名义什么都放过去。
  for (const [name, txt] of [['ci.yml', CI], ['scripts/preflight.sh', PF]]) {
    assert.match(
      txt,
      /没(有)?找到任何 \*\.test\.mjs/,
      `${name} 缺少「枚举到 0 个测试文件就报错」的守卫：目录选错或测试被删光时，` +
        `闸门会跑 0 个用例然后报全绿 —— 比没有闸门更危险。`,
    );
  }

  const disk = readdirSync(new URL('web/tests/', REPO)).filter((f) => f.endsWith('.test.mjs'));
  assert.ok(disk.length > 0, 'web/tests 下一个 *.test.mjs 都没有，上面的枚举断言等于空转');
});

test('tidy / gofmt 两道闸门本地与 CI 都要有', () => {
  for (const must of ['go mod tidy', 'gofmt -l']) {
    for (const [name, txt] of [['ci.yml', CI], ['scripts/preflight.sh', PF]]) {
      assert.ok(
        txt.includes(must),
        `${name} 里缺少「${must}」闸门：这道闸门在 CI 上是白红过的常客` +
          `（2026-09-13 就因为本地没跑 gofmt -l 白红一次），本地必须能提前跑到。`,
      );
    }
  }
});

test('每条注入自证脚本都必须在 CI 与 preflight 里被真正调用', () => {
  const groups = selfCheckGroups();
  const scripts = groups.flatMap(([, files]) => files);

  // 先守枚举本身，而且是**按组**守：某一组的 readdir 塌成 0 时，别的组照样把
  // 总数顶过全局下限，于是「整个目录自动免疫」会静默回来（实测过：老写法的全局
  // 下限对这种塌陷 0 报错）。哪一组空了都要红，并且要点出是哪一组。
  for (const [label, files] of groups) {
    assert.ok(
      files.length > 0,
      `枚举规则失效：${label} 一个文件都没收到 —— 这一组的尺子从此无人看守，` +
        `而症状是「一切正常」。要么是 readdir/后缀规则写错了，要么是这组真的没了（那要删掉这条枚举）。`,
    );
  }
  assert.ok(
    scripts.length >= 10,
    `只枚举到 ${scripts.length} 条自证脚本（期望 ≥10，各组实得：` +
      groups.map(([l, f]) => `${l}=${f.length}`).join('；') +
      `）。全局下限是防「枚举整体失效」的第二道，报「总数不够」没法归因，` +
      `所以归因请以上面那条按组断言为准。`,
  );

  for (const s of scripts) {
    assert.ok(
      CI.includes(s),
      `ci.yml 里没有调用 ${s} —— 这条防线在 CI 里从不跑，等于没有防线。` +
        `它会静静地一直绿（或一直悄悄报失效）直到有人哪天手动跑一次。`,
    );
    assert.ok(
      PF.includes(s),
      `scripts/preflight.sh 里没有调用 ${s} —— 本地闸门比 CI 少一道，` +
        `于是这类问题只能等推送之后由 CI 告诉你。`,
    );
  }
});

test('离线包装后探测自证：正向与两路注入都必须在 CI / preflight 里真跑', () => {
  const HARNESS = 'deploy/offline/tests/install_probe_test.sh';
  const esc = HARNESS.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');

  // 先守「文件真的在」：脚本被删掉时，下面的每一条 includes 都会失败得很难懂
  // （"没有调用 xxx"），而真正的问题是文件没了。这条断言让报错说人话。
  assert.ok(
    existsSync(new URL(HARNESS, REPO)),
    `${HARNESS} 不存在 —— 装后探测的尺子丢了。这条守卫会因此变成空转，所以先钉住它。`,
  );

  for (const [name, txt] of [['ci.yml', CI], ['scripts/preflight.sh', PF]]) {
    // 正向模式
    assert.ok(
      txt.includes(HARNESS),
      `${name} 里没有跑 ${HARNESS} —— 装后服务探测（/health 真功能判定）没有闸门，` +
        `2026-09-15 那次「端口在听但抽字全废、安装一路绿」的形态会原样回来。`,
    );

    for (const mode of INJECT_MODES) {
      // 注入模式必须在**同一行**上出现（`INJECT=1 bash xxx.sh`），
      // 不能只把 INJECT=1 写在别处的注释里 —— 那种"看着有、其实没传"正是
      // 本文件存在的理由（本地绿、CI 绿、注入从没生效过）。
      const re = new RegExp(`${mode}\\s+(?:bash\\s+)?${esc}([^\\n]*)`);
      const m = txt.match(re);
      assert.ok(
        m,
        `${name} 里没有以「${mode}」真跑 ${HARNESS} —— 这一路注入自证不会被执行，` +
          `它只会静静地腐烂（不跑的时候它既不报错也不变红）。`,
      );
      // 反向要求：不许把注入模式的红吞掉。注入模式的退出码 0 表示"确实按预期转红了"，
      // 所以 `|| true` / `continue-on-error` 会让"注入后根本没红"也变成通过 ——
      // 那正是负向自证最常见的假绿形态。
      const tail = m[1] || '';
      assert.ok(
        !/\|\|\s*true|continue-on-error/.test(tail),
        `${name} 里「${mode} ${HARNESS}」后面带了吞错写法（${tail.trim()}）—— ` +
          `注入模式返回非 0 意味着「没红 / 红错了 / 顺带把别人也判红」，必须让 CI 红，` +
          `不许用 || true 装作没事。`,
      );
    }
  }
});
