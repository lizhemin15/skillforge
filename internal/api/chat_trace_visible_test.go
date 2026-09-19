package api

import (
	"strings"
	"testing"
)

// 材料窗口里旁白**必须替换**，不许叠链。
//
// 用户原话：「中间可以流式输出思考的一些中间材料，现在一直卡着计时，用户体验不佳」。
// 把旁白改成报真进度（已产出 N 字）之后，如果窗口里旧的旁白不摘掉，用户看到的是
// 同一句话黏八遍 —— 那还是「一直在念同一句」，不是进度：
//
//	模型思考中…已产出 239 字（已 2s）模型思考中…已产出 571 字（已 5s）模型思考中…
//
// 线上 dump /tmp/mat_dump_final.json 里这一段是逐帧可见的，不是假想。
//
// 可自证性：把 visibleMaterial 改成直接 return mat（即「去掉裁剪」），下面断言立刻转红；
// 把旁白的前导 \n 去掉（见 agent/materialFilter.narration）则窗口里换行消失，
// 本函数退化成恒等 —— 所以本测试守的是「换行分隔 + 只取最后一段」这一对约定。
func TestVisibleMaterialKeepsOnlyNewestNarration(t *testing.T) {
	real := "· 引述规范：领导讲话必须使用直接引号且内容源自素材"
	mat := real +
		"\n模型思考中…已产出 239 字（已 2s）" +
		"\n模型思考中…已产出 571 字（已 5s）" +
		"\n模型思考中…已产出 896 字（已 7s）"

	got := visibleMaterial(mat)

	if n := strings.Count(got, "模型思考中"); n != 1 {
		t.Errorf("窗口里有 %d 条旁白，必须只剩最新 1 条 —— 旧旁白叠成链就是「复读」\n实际: %q", n, got)
	}
	if !strings.Contains(got, "896 字（已 7s）") {
		t.Errorf("留下的不是**最新**那条旁白，实际: %q", got)
	}
	if !strings.HasPrefix(got, "模型思考中") {
		// 旁白之后若又来了真材料，真材料应该照旧可见（下一条用例覆盖）；本用例里
		// 旁白是最后一件发生的事，所以用户该看到的就是这条旁白本身。
		t.Errorf("只剩旁白时，显示的应该是旁白本身，实际: %q", got)
	}
}

// 旁白之后又来了真材料：旁白必须被摘掉，用户看到的是真材料。
//
// 这段**按线上的真实顺序**搭：traceClock.Thinking 每来一片材料就走一次
// 「旁白？→ 追加 : 摘尾旁白再追加」。上次这里搭错了顺序（把材料直接粘在旁白后面
// 当成已经发生过裁剪），于是断言红在一个不存在的场景上 —— 自己造的场景自己不算数。
func TestVisibleMaterialShowsMaterialAfterNarration(t *testing.T) {
	// 全部走 appendMaterial —— 出货代码里 Thinking 调的就是它。
	mat := appendMaterial("", "· 首段交代时间地点主办方")              // 真材料
	mat = appendMaterial(mat, "\n模型思考中…已产出 239 字（已 2s）")    // 静默兜底
	mat = appendMaterial(mat, "\n模型思考中…已产出 571 字（已 5s）")    // 静默兜底（替换）
	mat = appendMaterial(mat, "· 取素材中具体企业或项目实例，用阿拉伯数字量化成果") // 真材料到了

	got := visibleMaterial(mat)
	if strings.Contains(got, "模型思考中") {
		t.Errorf("旁白之后已有更晚的真材料，用户该看到材料而不是旁白，实际: %q", got)
	}
	if !strings.Contains(got, "量化成果") {
		t.Errorf("新真材料被吃掉了，实际: %q", got)
	}
}

// 没有旁白时窗口原样透出：真材料抽段不含换行，这条保证裁剪不误伤真内容。
func TestVisibleMaterialWithoutNarrationIsIdentity(t *testing.T) {
	real := "· 5W1H，前120字内交代时间、地点、主办方、核心事件及意义"
	if got := visibleMaterial(real); got != real {
		t.Errorf("无旁白时不该裁剪\n期望: %q\n实际: %q", real, got)
	}
	if got := visibleMaterial(""); got != "" {
		t.Errorf("空窗口应返回空串，实际: %q", got)
	}
}
