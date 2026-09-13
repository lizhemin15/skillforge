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
import { readFileSync, readdirSync } from 'node:fs';

const REPO = new URL('../../', import.meta.url);
const read = (p) => readFileSync(new URL(p, REPO), 'utf8');

const CI = read('.github/workflows/ci.yml');
const PF = read('scripts/preflight.sh');

// 枚举仓库里所有「注入自证」脚本。它们的共同点是：往出货文件里注入真实故障、
// 要求对应断言变红、再显式还原。这类脚本有个致命性质 —— **不跑就没价值**，
// 而且不跑的时候它是静的（不会报错、不会腐烂到崩，只是永远绿）。
// 实测代价：scripts/category_guard_inject.py 的一条锚点跟实现脱钩很久，
// 脚本每次都在打印「锚点失效 ✗」，但它既不在 CI 也没人手动跑，谁都没看见。
const selfCheckScripts = () => [
  ...readdirSync(new URL('internal/api/', REPO)).filter((f) => f.endsWith('_mutation_check.sh')).map((f) => `internal/api/${f}`),
  ...readdirSync(new URL('web/tests/', REPO)).filter((f) => f.endsWith('_mutation_check.sh')).map((f) => `web/tests/${f}`),
  ...readdirSync(new URL('scripts/', REPO)).filter((f) => /inject.*\.py$/.test(f)).map((f) => `scripts/${f}`),
];

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
  const scripts = selfCheckScripts();

  // 先守枚举本身：数目对不上说明上面的 readdir 逻辑失效了，
  // 那样下面的循环会「空转通过」—— 这是所有守卫最常见的自杀方式。
  assert.ok(
    scripts.length >= 5,
    `只枚举到 ${scripts.length} 条自证脚本（期望 ≥5：internal/api 1 条、web/tests 2 条、scripts 2 条）。` +
      `枚举逻辑失效时这个测试会空转通过，所以必须把下限也钉死。实得：${scripts.join(', ')}`,
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
