package store

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// TemplateFile 描述一个「可被填充」的模板文件：技能目录里的 .docx / .xlsx。
//
// 它是 Agent 工具 fill_template 的输入，所以只暴露工具真正需要的东西
// （定位 + 展示名 + 格式），不把 store 的内部结构泄给 tools 包。
type TemplateFile struct {
	Slug   string // 所属技能 slug
	Rel    string // 相对技能目录的路径，如 "采购合同模板.docx" / "source/x.docx"
	Name   string // 展示名（文件名）
	Size   int64
	Format string // docx | xlsx
}

// ListTemplates 扫出所有技能的模板文件。
//
// 为什么不复用 ListFiles：ListFiles 是「技能知识包管理」的视图，只列白名单
// 内容（system_prompt.md / template.md / requirement.md / style_profile.md /
// examples/ / source/）。而模板文件恰恰在技能目录**顶层**
// （实测：采购合同/采购合同模板.docx、采购验收单/采购验收单模板.xlsx），
// 不在那份白名单里——用 ListFiles 会一个模板都找不到。
//
// 扫描范围：技能目录顶层 + source/。跳过 versions/（历史快照）、examples/
// （.md 案例）以及所有非 .docx/.xlsx 文件。技能目录不存在时按空处理，
// 不把「还没建技能」变成错误。
func (s *SkillStore) ListTemplates() ([]TemplateFile, error) {
	entries, err := os.ReadDir(s.skillsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []TemplateFile
	for _, en := range entries {
		if !en.IsDir() {
			continue
		}
		slug := en.Name()
		dir := filepath.Join(s.skillsDir, slug)
		// 顶层
		out = append(out, scanTemplateDir(slug, dir, "")...)
		// source/（用户上传的参考件也常被当模板用）
		out = append(out, scanTemplateDir(slug, filepath.Join(dir, "source"), "source/")...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Slug != out[j].Slug {
			return out[i].Slug < out[j].Slug
		}
		return out[i].Rel < out[j].Rel
	})
	return out, nil
}

// scanTemplateDir 扫一个目录里的 .docx/.xlsx，prefix 拼回相对路径。
func scanTemplateDir(slug, dir, prefix string) []TemplateFile {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []TemplateFile
	for _, en := range entries {
		if en.IsDir() || strings.HasPrefix(en.Name(), ".") {
			continue
		}
		format := templateFormatOf(en.Name())
		if format == "" {
			continue
		}
		fi, err := en.Info()
		if err != nil {
			continue
		}
		out = append(out, TemplateFile{
			Slug:   slug,
			Rel:    prefix + en.Name(),
			Name:   en.Name(),
			Size:   fi.Size(),
			Format: format,
		})
	}
	return out
}

// templateFormatOf 判断文件是否是可填模板，返回格式标记（"" 表示不是）。
// 只认现代 OOXML：docgen 的填充引擎走 ZIP+XML 重写，.doc/.xls 这类
// 二进制老格式无法解析，收进来只会让模型白调一次工具。
func templateFormatOf(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".docx":
		return "docx"
	case ".xlsx":
		return "xlsx"
	default:
		return ""
	}
}

// maxTemplateBytes 单个模板的读取上限。
// 服务以 root 运行，工具又是模型驱动的，所以这里不能只靠 safeRel ——
// 万一有人把 500MB 的文件丢进技能目录，一次 fill_template 就能把内存吃光。
const maxTemplateBytes = 32 << 20 // 32MB

// ReadTemplate 读出模板字节。
// 复用 safeRel 的防穿越（强制 rooted，杜绝 ../ 逃逸），并再次校验扩展名
// 与大小——工具只能读模板，不能顺着这条路读走技能目录里的其它文件。
func (s *SkillStore) ReadTemplate(slug, rel string) ([]byte, error) {
	format := templateFormatOf(rel)
	if format == "" {
		return nil, fmt.Errorf("不是可填模板（只支持 .docx/.xlsx）: %s", rel)
	}
	abs, err := s.absFile(slug, rel)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if fi.IsDir() {
		return nil, fmt.Errorf("是目录，不是模板文件: %s", rel)
	}
	if fi.Size() > maxTemplateBytes {
		return nil, fmt.Errorf("模板过大（%d 字节，上限 %d）: %s", fi.Size(), maxTemplateBytes, rel)
	}
	return os.ReadFile(abs)
}
