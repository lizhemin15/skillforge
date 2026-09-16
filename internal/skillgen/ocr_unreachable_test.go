package skillgen

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lizhemin15/skillforge/internal/ocrsvc"
)

// 本文件锁死投诉现场：训练技能时报
//
//	127.0.0.1:8093/extract connection refused
//
// 两个必须成立的产品性质：
//  1. 失败要**快**——解析客户端默认上限 30 分钟，探活/连接失败绝不能把用户按在那儿等；
//  2. 失败要**可执行**——必须出现 systemctl 命令，且同一段指引只出现一次
//     （上传 3 份文档不该把同一段话印 3 遍）。
//
// 注入会红的方式：
//   - 去掉 ingestFiles 里的预检：TestIngestFilesEmitsPreflightHint 那条「steps 里有 systemctl」会红；
//   - 把 EnvHint 拼进每条 Failures：TestEnvHintNotDuplicatedAcrossFiles 的计数会红；
//   - 恢复「原样抛 err」：TestIngestFilesFastFailsWithEnvHint 的可执行断言会红。

// deadOCRURL 返回一个没有监听者的地址（先占后关），保证真拿到 refused。
func deadOCRURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占端口失败：%v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return "http://" + addr
}

func deadOCRInput(files ...string) *Input {
	in := &Input{Name: "投诉复现"}
	for _, fn := range files {
		in.Files = append(in.Files, &UploadedFile{Filename: fn, Content: binaryPDFBytes()})
	}
	return in
}

// TestIngestFilesFastFailsWithEnvHint：服务没在运行 ⇒ 快速失败 + 可执行指引。
func TestIngestFilesFastFailsWithEnvHint(t *testing.T) {
	svc := deadOCRURL(t)
	g := &Generator{}
	g.SetOCR(svc)

	var steps []string
	start := time.Now()
	rep := g.ingestFiles(context.Background(), deadOCRInput("手册.pdf"), func(s string) { steps = append(steps, s) })
	elapsed := time.Since(start)

	if rep.DocFiles != 1 || rep.OKFiles != 0 {
		t.Fatalf("素材统计不对：DocFiles=%d OKFiles=%d（应为 1/0）", rep.DocFiles, rep.OKFiles)
	}
	// ① 快：解析客户端默认上限是 30 分钟，这里必须毫秒级返回。
	// 注入会红的方式：把探活/连接超时写死成默认解析超时 → 本断言超时红。
	if elapsed > 3*time.Second {
		t.Fatalf("解析服务连不上，却花了 %s 才报错：用户会被按在这里等", elapsed)
	}
	// ② 可执行：必须出现 systemctl 修复命令（出现在 EnvHint 里）。
	if !strings.Contains(rep.EnvHint, "systemctl restart") {
		t.Errorf("EnvHint 里没有可执行命令：%q", rep.EnvHint)
	}
	if !strings.Contains(rep.EnvHint, ocrsvc.Endpoint(svc)) {
		t.Errorf("EnvHint 没报出真实地址 %s：%q", ocrsvc.Endpoint(svc), rep.EnvHint)
	}
	// ③ 逐文件那行必须短：长指引只存一份（见 EnvHint 注释）。
	for _, f := range rep.Failures {
		if strings.Contains(f, "\n") {
			t.Errorf("逐文件失败原因里塞了多行长文：%q", f)
		}
		if !strings.Contains(f, "没有在运行") {
			t.Errorf("逐文件失败原因没说清「服务没在运行」：%q", f)
		}
	}
}

// TestIngestFilesEmitsPreflightHint：预检必须在**长解析之前**给出一条人话，
// 这样即使用户的文件本来也读不出字，他第一眼看到的仍是环境结论。
// 注入会红的方式：删掉 ingestFiles 里的 ocrsvc.Check 预检块。
func TestIngestFilesEmitsPreflightHint(t *testing.T) {
	svc := deadOCRURL(t)
	g := &Generator{}
	g.SetOCR(svc)

	var steps []string
	g.ingestFiles(context.Background(), deadOCRInput("手册.pdf"), func(s string) { steps = append(steps, s) })

	hint := ""
	for _, s := range steps {
		if strings.Contains(s, "systemctl restart") {
			hint = s
		}
	}
	if hint == "" {
		t.Fatalf("进度流里没有任何可执行提示，用户只能看到「解析失败」：%v", steps)
	}
	if !strings.Contains(hint, "没有在运行") && !strings.Contains(hint, "运行时已损坏") {
		t.Errorf("预检提示没说清是哪一种坏态：%q", hint)
	}
}

// TestEnvHintNotDuplicatedAcrossFiles：上传多份文档时，修复指引只出现一次。
// 这是「3 份文档 = 3 遍同样的话」的回归钉子。
func TestEnvHintNotDuplicatedAcrossFiles(t *testing.T) {
	svc := deadOCRURL(t)
	g := &Generator{}
	g.SetOCR(svc)
	rep := g.ingestFiles(context.Background(), deadOCRInput("A.pdf", "B.pdf", "C.pdf"), nil)

	if rep.DocFiles != 3 {
		t.Fatalf("DocFiles=%d，期望 3", rep.DocFiles)
	}
	err := g.enforceMaterialGate(rep)
	if err == nil {
		t.Fatal("三份文档全部解析失败，门禁竟然放行（会生成与素材无关的技能）")
	}
	msg := err.Error()
	if !strings.Contains(msg, "本次训练已中止") {
		t.Errorf("中止信息缺失：%q", msg)
	}
	if n := strings.Count(msg, "systemctl restart"); n != 1 {
		t.Errorf("修复指引出现 %d 次（应为 1 次）：%q", n, msg)
	}
	if n := strings.Count(msg, "文档解析环境异常"); n != 1 {
		t.Errorf("环境结论出现 %d 次（应为 1 次）：%q", n, msg)
	}
}

// TestNoPreflightForTextOnlyMaterial：纯文本素材不该为探活白等一次 HTTP。
// 用计数桩来证：文本素材跑完，桩上必须是 0 次请求。
func TestNoPreflightForTextOnlyMaterial(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		fmt.Fprint(w, `{"ok":true,"runtime_ok":true,"text":"x"}`)
	}))
	defer srv.Close()

	g := &Generator{}
	g.SetOCR(srv.URL)
	in := &Input{Name: "纯文本", Files: []*UploadedFile{{Filename: "大纲.md", Content: "第一章 写作要求\n内容若干"}}}
	rep := g.ingestFiles(context.Background(), in, nil)

	if got := atomic.LoadInt64(&hits); got != 0 {
		t.Errorf("纯文本素材却对解析服务发了 %d 次请求：预检条件写错了", got)
	}
	if rep.DocFiles != 0 || rep.EnvHint != "" {
		t.Errorf("纯文本素材不该产生文档解析结论：%+v", rep)
	}
}

// TestSelfHealingResponseIsNotBlamedOnTheFile：解析服务自报「正在自愈」时，
// 必须告诉用户「重试即可、文件没问题」，而不是把内部文案原样抛出。
// 注入会红的方式：删掉 ocrExtract 里的 ocrsvc.SelfHealing 分支。
func TestSelfHealingResponseIsNotBlamedOnTheFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":false,"error":"运行时目录已丢失","runtime_ok":false,"self_healing":true}`)
	}))
	defer srv.Close()

	g := &Generator{}
	g.SetOCR(srv.URL)
	rep := g.ingestFiles(context.Background(), deadOCRInput("手册.pdf"), nil)

	if len(rep.Failures) == 0 {
		t.Fatal("自愈回包应当被记成失败")
	}
	joined := strings.Join(rep.Failures, "；")
	if !strings.Contains(joined, "重试") || !strings.Contains(joined, "文件本身没有问题") {
		t.Errorf("没有告诉用户「稍后重试、文件没问题」：%q", joined)
	}
}
