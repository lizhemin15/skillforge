package api

import (
	"errors"
	"net/http"

	"github.com/lizhemin15/skillforge/internal/store"
)

// 分类结构管理（对**所有**技能开放；手册技能与非手册技能的区别只在于分类从哪来）。
//
// 为什么要有这层：训练期抽出来的分类是手册的骨架，但真实使用中会碰到
// 「手册里没有、我们单位常写的那类稿子」——用户只能干瞪眼，或者去手改磁盘文件。
// 手改是很危险的：分类名同时散落在 6 个地方（见 store.RenameCategory），
// 漏一个就是「界面上看着改了、运行时模型还在按旧分类名找类」的静默失效。
// 所以增删改一律走后端，由后端做整段锚定的级联改写。
//
// 「只给手册技能开」这条限制已经取消（产品拍板）——理由和代价见 store.CreateCategory
// 的注释：非手册技能建分类时会自动补出 categories/ 骨架，技能就此从
// 「一步直执笔」变成「判类 → 按类执笔 → 审稿」三段，这个后果通过
// CategoryChange.notes 回给前端说明，而不是拦着不让建。
//
// 三个动作的共同点：改完立刻读回生效——categories/*.md 就是运行时读的那份文件，
// 不另存 JSON 副本。这样「界面上能编辑」和「运行时用得上」是同一份数据。

// CreateSkillCategory 新增一个分类（建分类文件 + 范文目录 + 两张路由表补行）。
//
// 只建骨架、不塞范文：管理员接着用已有的「上传范文」往这个类里加稿子。
// 这样新分类一开始是空的，运行时会明确告诉模型「本类暂无范文」，
// 而不是拿别的类的范文凑数。
func (a *Admin) CreateSkillCategory(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	var body struct {
		Name        string `json:"name"`
		Trigger     string `json:"trigger"`
		Requirement string `json:"requirement"`
	}
	if err := readBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体无效")
		return
	}
	ch, err := a.store.CreateCategory(slug, body.Name, body.Trigger, body.Requirement)
	if err != nil {
		writeErr(w, categoryErrStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "change": ch})
}

// RenameSkillCategory 给分类改名。
//
// 前端必须做二次确认，因为这一下会同时改 6 个地方（分类文件、范文目录、
// 两张路由表、reviewer.md 小节、meta.json 清单）。范文原文（examples/、source/）
// 一个字都不动——那是手册原文，用户定的硬约束。
//
// 只改显示名也走这个接口（file 不变、newName 传同一个名字）：
// 容忍同名是必要的，因为 H1 有可能本来就和文件名不一致。
func (a *Admin) RenameSkillCategory(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	var body struct {
		File    string `json:"file"`
		NewName string `json:"new_name"`
	}
	if err := readBody(r, &body); err != nil || body.File == "" {
		writeErr(w, http.StatusBadRequest, "需要 file 与 new_name")
		return
	}
	ch, err := a.store.RenameCategory(slug, body.File, body.NewName)
	if err != nil {
		writeErr(w, categoryErrStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "change": ch})
}

// DeleteSkillCategory 删除一个分类及其范文目录。
//
// 分类下还有范文时必须显式 force —— 这不是多余的确认，而是防误删：
// 一个类下面可能挂着十几篇手册原文，删掉无法从界面上恢复。
// 空分类（没有范文）不需要 force，避免管理员为了删个空壳还得点两次。
func (a *Admin) DeleteSkillCategory(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	file := r.URL.Query().Get("file")
	if file == "" {
		writeErr(w, http.StatusBadRequest, "需要 file")
		return
	}
	force := r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true"
	ch, err := a.store.DeleteCategory(slug, file, force)
	if err != nil {
		// 有范文时的「未 force」不是失败，是**待确认**：store 已经把拟定好的变更
		// （会删哪几个文件、几篇范文）放进 ch 一起返回了。这里必须把它翻成
		// 机器可读的 need_force / example_count —— 前端要据此弹第二次确认。
		// 不靠前端匹配错误文案：文案是给人看的、会改，一旦匹配不上，
		// 用户点「删除」只会得到一句「删除失败」，永远走不到「确认再删」那一步，
		// 有范文的分类从此在界面上删不掉。
		if errors.Is(err, store.ErrCategoryInUse) {
			resp := map[string]any{"error": err.Error(), "need_force": true}
			if ch != nil {
				resp["example_count"] = ch.ExampleCount
				resp["change"] = ch
			}
			writeJSON(w, http.StatusBadRequest, resp)
			return
		}
		writeErr(w, categoryErrStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "change": ch})
}

// categoryErrStatus 把 store 的错误分成两类：
//   - 用户能自己修的（名字非法、重名、目标已存在、找不到分类、要 force）→ 400。
//     前端直接把消息显示出来，用户改一下输入就能成功；
//   - 其它（读盘/写盘失败）→ 500，那是服务端的事，不该让用户以为是自己的输入问题。
//
// 判类型靠错误链（errors.Is）而不是匹配文案：文案改一次字符串匹配就悄悄失效，
// 失效的表现是 500——用户看到「服务器错误」去翻日志，而真正原因是自己名字填错了。
//
// ErrNotManualSkill 曾经也在这个 400 列表里；方案 C 放开后 store 不再抛它，
// 所以这里同步删掉——留着一个永不命中的分支，下一个人读代码会以为
// 「非手册技能还是被拦着」，从而写出错误的判断。
func categoryErrStatus(err error) int {
	switch {
	case errors.Is(err, store.ErrCategoryBadInput),
		errors.Is(err, store.ErrCategoryInUse):
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}
