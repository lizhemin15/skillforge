package store

import (
	"encoding/json"
	"log"
	"path/filepath"
	"strings"

	"github.com/lizhemin15/skillforge/internal/model"
)

// 本文件负责 skills 表里「可下发附件清单」的编解码。
//
// 历史背景：附件原本是**单个字符串**（`attachment` 列，值是相对技能目录的路径），
// 语义是「管理员单独上传的那一整份 office 文件」。真实场景里源文件内嵌的模板
// 往往是**多份**（正文写着「附件：1.报名表 2.汇总表」），单值字段只能表达其中
// 一份，用户拿到的清单是残缺的。所以新增 `attachments` JSON 数组列。
//
// 两列并存不是双事实来源，而是「读兼容 + 写单源」：
//   - 读：数组为空且旧列非空 → 折成数组的唯一一项（decodeAttachments），
//     于是**运行时代码只认 Attachments**，不必到处分叉判断；
//   - 写：数组有内容 → 旧列清空（encodeAttachments），避免回滚到旧二进制时
//     又冒出一份陈旧的副本当真。
//
// 没做数据迁移是刻意的：旧行原地就能读对，迁移脚本反而多一个可能改坏线上数据的环节。

// decodeAttachments converts the `attachments` JSON column into []Attachment and
// folds the legacy single-value `attachment` column in when the array is empty.
func decodeAttachments(raw, legacy string) []model.Attachment {
	var parsed []model.Attachment
	if s := strings.TrimSpace(raw); s != "" {
		if err := json.Unmarshal([]byte(s), &parsed); err != nil {
			// 坏 JSON 不该把整个技能变成「读不出来」：附件是附加品，技能本体
			// （提示词/示例）才是主商品。退回「无附件」并留日志，别让一行脏
			// 数据把分类器里那个技能整格打成错误。
			log.Printf("store: skills.attachments JSON 解析失败(按无附件处理): %v", err)
			parsed = nil
		}
	}
	out := make([]model.Attachment, 0, len(parsed))
	for _, a := range parsed {
		rel := strings.TrimSpace(a.Rel)
		if rel == "" {
			// 没有 Rel 就拼不出下载地址，下发必然是死链。宁可少一条，
			// 也不要给用户一个点开是 404/空文件的条目。
			continue
		}
		if a.Name == "" {
			a.Name = filepath.Base(rel)
		}
		a.Rel = rel
		out = append(out, a)
	}
	if len(out) == 0 {
		if l := strings.TrimSpace(legacy); l != "" {
			out = append(out, model.Attachment{Name: filepath.Base(l), Rel: l, How: "legacy"})
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// encodeAttachments is the write side. It returns the JSON for the array column
// plus the value to store in the legacy column: as soon as the array carries
// entries the legacy column is cleared, so the two columns can never disagree
// about which files a skill delivers.
func encodeAttachments(atts []model.Attachment, legacy string) (string, string) {
	kept := make([]model.Attachment, 0, len(atts))
	for _, a := range atts {
		rel := strings.TrimSpace(a.Rel)
		if rel == "" {
			continue
		}
		if a.Name == "" {
			a.Name = filepath.Base(rel)
		}
		a.Rel = rel
		kept = append(kept, a)
	}
	if len(kept) == 0 {
		return "", strings.TrimSpace(legacy)
	}
	b, err := json.Marshal(kept)
	if err != nil {
		log.Printf("store: 序列化 attachments 失败(按无附件写入): %v", err)
		return "", strings.TrimSpace(legacy)
	}
	return string(b), ""
}
