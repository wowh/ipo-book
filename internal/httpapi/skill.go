package httpapi

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// templates 是 skill 包的原始文件。
//
// 内容放成文件而不是字符串常量：里面有大量 markdown 代码块与反引号，写在
// Go 源码里需要处处转义，很容易写错并且没法直接预览。
//
//go:embed templates
var templates embed.FS

// skillEnv 的占位符。导出时按当前机器替换，代码与端口因机器而异。
const (
	phBaseURL       = "{{baseUrl}}"
	phDataPath      = "{{dataPath}}"
	phSkillDir      = "{{skillDir}}"
	phDiscoveryPath = "{{discoveryPath}}"
	phDiscoveryName = "{{discoveryName}}"
	phVersion       = "{{version}}"
	phExportedAt    = "{{exportedAt}}"
)

// ExportSkill 把 agent 用的 skill 包写到 dir。
//
// 包内三份文件：SKILL.md 是使用说明（带 frontmatter，支持 Agent Skill 的客户端
// 可以直接加载），references/api.md 是接口与字段速查，scripts/ipobook.sh 是读
// 发现文件直接 curl 的封装脚本。
func (s *Server) ExportSkill(dir string) error {
	return s.ExportSkillAt(dir, s.BaseURL())
}

// ExportSkillAt 用指定的接口地址生成 skill 包。
//
// baseURL 允许调用方给出「本进程没有监听、但确实在跑的实例」的地址，例如
// 命令行导出时复用已有实例写下的发现文件，避免为了写一份说明去抢端口。
func (s *Server) ExportSkillAt(dir, baseURL string) error {
	if dir == "" {
		dir = s.SkillDir()
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("目录无效：%w", err)
	}
	if baseURL == "" {
		baseURL = "（应用未启动，以 " + DiscoveryName + " 里的 baseUrl 为准）"
	}
	replacer := strings.NewReplacer(
		phBaseURL, baseURL,
		phDataPath, s.DataPath(),
		phSkillDir, abs,
		phDiscoveryPath, s.DiscoveryPath(),
		phDiscoveryName, DiscoveryName,
		phVersion, s.version,
		phExportedAt, time.Now().Format("2006-01-02 15:04"),
	)

	root, err := fs.Sub(templates, "templates")
	if err != nil {
		return err
	}
	return fs.WalkDir(root, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(root, name)
		if err != nil {
			return err
		}
		path := filepath.Join(abs, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("创建目录失败：%w", err)
		}
		mode := os.FileMode(0o644)
		if filepath.Ext(name) == ".sh" {
			mode = 0o755
		}
		if err := os.WriteFile(path, []byte(replacer.Replace(string(data))), mode); err != nil {
			return fmt.Errorf("写入 %s 失败：%w", name, err)
		}
		return nil
	})
}
