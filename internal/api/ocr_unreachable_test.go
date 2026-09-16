package api

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
)

// 本文件锁住**另一条** ocrd 调用点：管理端「上传素材并解析」（Admin.extractDoc）。
// 两条通道必须给出同一档人话 —— 只修训练通道的话，用户换个入口上传照样会看到
// 一行 Post "http://127.0.0.1:8093/extract": dial tcp ...: connection refused。
//
// 注入会红的方式：把 extractDoc 里的 ocrsvc.Explain 去掉（改回 return res, err）
// → TestExtractDocDeadServiceSaysWhatToDo 的断言全红。

// deadOCRURL 返回没有监听者的地址（先占后关），保证真拿到 ECONNREFUSED。
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

// TestExtractDocDeadServiceSaysWhatToDo：解析服务没在运行 ⇒ 上传解析报的是「去修环境」。
func TestExtractDocDeadServiceSaysWhatToDo(t *testing.T) {
	h := newTestHandler(t)
	svc := deadOCRURL(t)
	h.Admin.ocrURL = svc

	_, err := h.Admin.extractDoc("手册.pdf", []byte("%PDF-1.4 fake"))
	if err == nil {
		t.Fatal("对没有监听者的端口解析竟然成功了")
	}
	msg := err.Error()
	if !strings.Contains(msg, "systemctl restart") {
		t.Errorf("上传解析失败却没给出可执行命令：%q", msg)
	}
	if !strings.Contains(msg, "systemctl") || !strings.Contains(msg, "journalctl") {
		t.Errorf("排障入口不全：%q", msg)
	}
	if strings.Contains(msg, "dial tcp") && !strings.Contains(msg, "原始错误") {
		t.Errorf("底层噪音没有被收进带标签的附录：%q", msg)
	}
	// 原始错误仍要可判定（排障要靠它区分 refused / DNS）。
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("Explain 之后丢失了原始 syscall 错误：%v", err)
	}
}

// TestExtractDocSelfHealingIsRetryable：解析服务自报「正在自愈」时，
// 必须告诉用户「稍后重试」，而不是把内部错误文案抛出去。
// 注入会红的方式：删掉 extractDoc 里的 out.SelfHealing 分支。
func TestExtractDocSelfHealingIsRetryable(t *testing.T) {
	h := newTestHandler(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":false,"error":"运行时目录已丢失","runtime_ok":false,"self_healing":true}`)
	}))
	defer srv.Close()
	h.Admin.ocrURL = srv.URL

	_, err := h.Admin.extractDoc("手册.pdf", []byte("%PDF-1.4 fake"))
	if err == nil {
		t.Fatal("自愈回包应当算失败（本次没解析出东西）")
	}
	if !strings.Contains(err.Error(), "重试") || !strings.Contains(err.Error(), "文件本身没有问题") {
		t.Errorf("没有告诉用户「稍后重试、文件没问题」：%v", err)
	}
	if strings.Contains(err.Error(), "解析服务: ") {
		t.Errorf("还是把内部错误文案原样抛出了：%v", err)
	}
}

// TestExtractDocContentErrorUnchanged：普通解析失败（加密 PDF 之类）不许被套上环境指引，
// 否则用户会去重启服务，而真正该换的是文件。
// 注入会红的方式：把 Unreachable 判定放宽成 err != nil。
func TestExtractDocContentErrorUnchanged(t *testing.T) {
	h := newTestHandler(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":false,"error":"这份 PDF 已加密"}`)
	}))
	defer srv.Close()
	h.Admin.ocrURL = srv.URL

	_, err := h.Admin.extractDoc("手册.pdf", []byte("%PDF-1.4 fake"))
	if err == nil {
		t.Fatal("ok:false 应当算失败")
	}
	if strings.Contains(err.Error(), "systemctl") {
		t.Errorf("文件类错误被套上了环境指引，用户会被带偏：%v", err)
	}
	if !strings.Contains(err.Error(), "已加密") {
		t.Errorf("原始原因没透出来：%v", err)
	}
}
