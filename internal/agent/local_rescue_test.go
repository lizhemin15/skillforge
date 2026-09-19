package agent

import (
	"strings"
	"testing"
	"time"
)

// 意图识别彻底失败时的本地兜底路由 —— 钉死「什么时候猜、什么时候不猜」。
//
// 为什么这个判据值得一条回归防线：它的触发场景是**上游挂了**（分类跳 60s 超时、
// 两次都失败）。线上 2026-09-20 第 2 轮「把上面那篇整理成 Word」就是在那个场景下
// 变成 0 交付物的——用户看到的是「它不管我上文」，真因是分类跳挂掉后直接降级成
// 通用写作。这条路的正确行为不是「更聪明」，而是「在模型不可用时仍然做对最保守
// 的那一步」，且**只在证据齐全时才动手**：猜错会把一篇新话题的稿子塞进 Word。
//
// 判据的两条本地事实（与 doc_passthrough.go 的直通兜底共用同一套词表与门槛）：
//  1. 本轮是搬运/转格式意图（wantsCarryOver）；
//  2. 历史里确有长文产物（≥ passthroughMinArtifactRunes）。

// longArtifact 造一份「够长」的上一轮产物正文。
func longArtifact() string {
	return strings.Repeat("公司新闻稿正文内容。", 40) // 400 字 > 200 门槛
}

func artifactHistory(body string) []Message {
	return []Message{
		{Role: "user", Content: "按素材写一篇新闻稿", At: time.Now()},
		{Role: "assistant", Content: body, SkillSlug: "公司新闻通稿", Kind: KindArtifact, At: time.Now()},
	}
}

func TestRescueDecision(t *testing.T) {
	cases := []struct {
		name    string
		user    string
		history []Message
		want    bool
		why     string
	}{
		{
			name:    "长产物 + 整理成 Word ⇒ 认（线上那条路）",
			user:    "把上面这篇新闻稿原样整理成 Word 文档（.docx），正文一字不改。",
			history: artifactHistory(longArtifact()),
			want:    true,
			why:     "两条本地事实都成立：搬运意图 + 历史长产物",
		},
		{
			name:    "长产物 + 导出成 PDF ⇒ 认（同义说法也得覆盖）",
			user:    "把上面那篇导出成 PDF 给我",
			history: artifactHistory(longArtifact()),
			want:    true,
			why:     "「导出」在搬运词表里",
		},
		{
			name:    "长产物 + 全新话题 ⇒ 不认",
			user:    "再帮我写一篇关于新能源汽车的行业观察",
			history: artifactHistory(longArtifact()),
			want:    false,
			why:     "新话题：不能因为历史里有长文就把它的输出夺走塞进 Word",
		},
		{
			name:    "长产物 + 要改内容（润色）⇒ 不认",
			user:    "把上面那篇润色一下，扩写到 2000 字",
			history: artifactHistory(longArtifact()),
			want:    false,
			why:     "「润色/扩写」要动内容，不属于搬运；词表刻意不收",
		},
		{
			name:    "短产物 + 整理成 Word ⇒ 不认",
			user:    "把上面那篇整理成 Word",
			history: artifactHistory("好的，已为你生成。"),
			want:    false,
			why:     "历史里没有可搬运的长文，搬什么？",
		},
		{
			name:    "无历史 + 整理成 Word ⇒ 不认",
			user:    "把上面那篇整理成 Word",
			history: nil,
			want:    false,
			why:     "首轮就没有「上面那篇」，别猜",
		},
		{
			name: "只有用户消息（没有 assistant 产物）+ 整理成 Word ⇒ 不认",
			user: "把上面那篇整理成 Word",
			history: []Message{
				{Role: "user", Content: strings.Repeat("素材", 300), At: time.Now()},
			},
			want: false,
			why:  "用户给的素材不是产物：判据看的是 assistant 产物消息",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := rescueDecision(c.user, c.history)
			if got != c.want {
				t.Fatalf("rescueDecision=%v 期望 %v（%s）", got, c.want, c.why)
			}
		})
	}
}

// TestRescueDecisionBoundary 钉住门槛本身：比 passthroughMinArtifactRunes 少一个字就必须不认。
// 为什么单独测边界：判据用 >= 比较，差一个字符的偏差在真实语料里永远不会被人发现，
// 但它决定了「一条十来字的回执」和「一篇真稿子」的界线。
func TestRescueDecisionBoundary(t *testing.T) {
	msg := "把上面那篇整理成 Word"
	just := strings.Repeat("字", passthroughMinArtifactRunes)
	if !rescueDecision(msg, artifactHistory(just)) {
		t.Fatalf("恰好 %d 字（== 门槛）应当认", passthroughMinArtifactRunes)
	}
	short := strings.Repeat("字", passthroughMinArtifactRunes-1)
	if rescueDecision(msg, artifactHistory(short)) {
		t.Fatalf("少一个字（%d）就不该认：门槛是 >= %d", passthroughMinArtifactRunes-1,
			passthroughMinArtifactRunes)
	}
}

// TestRescueDecisionIgnoresMachineMarkers 钉住「去空白 + 剥机器标记」这层：
// 产物消息尾部挂着给下一轮回放的规格 JSON，它不是正文。若把它算进长度，
// 一条只有「已生成规格:{…}」这种机器尾巴的消息就能凑够门槛，触发一次错路由。
func TestRescueDecisionIgnoresMachineMarkers(t *testing.T) {
	body := strings.Repeat("字", passthroughMinArtifactRunes-1)
	machine := body + "\n" + docSpecMarker + `{"format":"word","paras":["` +
		strings.Repeat("字", 500) + `"]}`
	if rescueDecision("把上面那篇整理成 Word", artifactHistory(machine)) {
		t.Fatalf("机器标记不该计入「长文产物」的长度（正文只有 %d 字）",
			passthroughMinArtifactRunes-1)
	}
}
