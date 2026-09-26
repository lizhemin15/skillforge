package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lizhemin15/skillforge/internal/agent"
	"github.com/lizhemin15/skillforge/internal/llm"
	"github.com/lizhemin15/skillforge/internal/model"
)

// 这一条守的是「技能加载失败不能整轮报错」—— 用户屏幕上不许再出现
// 「⚠ sql: no rows in result set」这种数据库实现细节（2026-09-26 线上原样截图）。
//
// 为什么必须是行为级的整链测试，而不是读源码断言行号：
// 同一个故障有三条来路，只有真跑一轮 SSE 才同时盖住 ——
//
//	a) 分类器编了个不在库里的 slug（L2 守卫中和，走 agent 那组单测）
//	b) 库里有行、但磁盘上的技能内容没了（并发删除 / 部署拷漏 / 手动 rm）
//	c) 存储层把 sql.ErrNoRows 原样上抛（store 那组单测钉契约）
//
// 这里造 (b)：**DB 有行 + 目录被抽走**。它刻意绕开 L2（清单从 DB 读，看着一切正常），
// 所以只有 L3 兜底能救 —— 而旧实现在这一格是 write(evError, lerr.Error())，
// 整轮以错误帧收尾、零正文。
func TestChatMissingSkillContentDegradesToPlainWriteNotError(t *testing.T) {
	const target = "技能工厂"
	const body = "这是通用写作兜底产出的正文。"

	dir := t.TempDir()
	sk := newStoreForTest(t, dir)

	// 前提：技能在库里、且是启用状态（所以 buildRoster 会把它印给分类器，L2 不会拦）。
	if s, err := sk.Get(target); err != nil || s == nil {
		t.Fatalf("前提不成立：%q 不在库里（%v）—— 那样走的是 L2 那条路，这条尺子就白测了", target, err)
	}
	// 抽走磁盘内容：复刻「DB 有行、文件没了」。这是 L2 挡不住、只能靠 L3 兜的那一类。
	if err := os.RemoveAll(filepath.Join(sk.SkillsDir(), target)); err != nil {
		t.Fatalf("无法抽走技能目录: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sk.SkillsDir(), target, "system_prompt.md")); !os.IsNotExist(err) {
		t.Fatalf("前提不成立：system_prompt.md 还在，加载不会失败（err=%v）", err)
	}

	f := newFakeLLM(t, func(system, user string) string {
		if strings.Contains(system, "多智能体管线的调度器") {
			return `{"intent":"write","action":"write","skill_slug":"` + target + `",` +
				`"reason":"题材命中","needs_tools":false}`
		}
		return body
	})
	h := &chatHandler{
		eng:      agent.New(llm.New(&model.LLMConfig{APIKey: "test", BaseURL: f.srv.URL, Model: "fake"}), sk),
		maxRound: 1,
	}

	sse := runRouteChat(t, h, `{"session_id":"s-missing","message":"帮我写一份园区开放日的宣传文案","mode":"auto"}`)
	frames := routeFrames(sse)

	// 前提：真走到了执笔。否则「没有错误帧」可能只是整条链在更早处就断了（空跑绿）。
	if !anyRequestSystemContains(f, target) {
		t.Fatalf("前提不成立：分类请求里没出现 %q —— 它不在可用清单里，那测的是 L2 不是 L3", target)
	}

	// A1：整轮不许以错误帧收尾。**这条排在最前面**：它就是用户在 2026-09-26 看到的东西，
	// 必须永远是最先报出来的那条。放在「执笔发生过」之后会被那条前提抢答 ——
	// 而「链路早断」本身正是这个故障的症状，让症状盖掉病因会把人指去错方向。
	if d := findFrameData(frames, evError); d != "" {
		t.Fatalf("技能内容缺失时整轮报了错（error 帧=%s）—— 用户要的是稿子，不是错误提示；"+
			"场景 (b) 完全可降级到通用写作", d)
	}

	// 前提（反向）：链路真的接着往下走了，没有被静默掐断。
	// 「没有错误帧」也可能是「什么都没发生」——那同样不是降级成功。
	if !anyRequestNotSystem(f, "多智能体管线的调度器") {
		t.Fatalf("前提不成立：只有分类那一跳发过请求，执笔从没发生 —— " +
			"降级后必须真去通用写作，而不是静默收摊")
	}
	// A2：存储实现细节不许出现在协议里（线上那一行就是它）。
	if strings.Contains(sse, "sql:") {
		t.Fatalf("SSE 里出现了 sql 实现细节，会被整段播给用户:\n%s", clipSSE(sse))
	}
	if strings.Contains(sse, "no rows") {
		t.Fatalf("SSE 里出现了 no rows 文案，会被整段播给用户:\n%s", clipSSE(sse))
	}

	// A3：降级必须出声，且要点名是谁、讲清改走了什么路。
	// 不看最后一帧 meta（后面可能还有别的 meta 帧），把全部 meta 帧拼起来判。
	metas := allFrameData(frames, evMeta)
	if !strings.Contains(metas, target) {
		t.Fatalf("降级说明没点名出问题的技能（meta=%q）—— 用户不知道是哪个技能坏了"+
			"（同一轮里还有别的 meta 帧，只有点名才分得清）", metas)
	}
	if !containsAny(metas, "读不出", "加载失败", "找不到", "缺失", "已被删除", "不存在") {
		t.Fatalf("降级说明没讲原因（meta=%q）—— 用户要的是「为什么这轮没按我的技能写」", metas)
	}
	if !strings.Contains(metas, "通用写作") {
		t.Fatalf("降级说明没讲清改走了哪条路（meta=%q）", metas)
	}

	// A4：真有正文产出。否则「没报错」可能只是静默吞掉、一个字都没写。
	if !strings.Contains(deltaText(frames), body) {
		t.Fatalf("降级后没有正文产出（delta=%q）—— 从「报错」变成「静默失败」更糟："+
			"用户既没有稿子也看不到原因", deltaText(frames))
	}
}

// allFrameData 拼接某一类事件的全部帧。
// 为什么需要它：findFrameData 只取最后一帧，而「技能加载失败」的降级说明不是本轮
// 最后一帧 meta（后面还有别的），取最后一帧会让正确实现也判红 —— 尺子锚错了对象。
func allFrameData(frames []routeFrame, event string) string {
	var b strings.Builder
	for _, fr := range frames {
		if fr.event == event {
			b.WriteString(fr.data)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// clipSSE 只用于报错信息，避免把整段 SSE 灌进测试日志。
func clipSSE(s string) string {
	r := []rune(s)
	if len(r) <= 600 {
		return s
	}
	return string(r[:300]) + "\n……（中略）……\n" + string(r[len(r)-300:])
}
