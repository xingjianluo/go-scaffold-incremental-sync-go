package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

// mapSource 把内存中的 map 作为配置来源（命令行参数、默认值都可用它表达）。
type mapSource struct {
	name string
	set  map[string]any
}

func (s *mapSource) Name() string { return s.name }
func (s *mapSource) Load() (*Layer, error) {
	return &Layer{Set: s.set}, nil
}

// Defaults 返回内置默认值来源，优先级最低，应第一个注册。
func Defaults() Source {
	return &mapSource{name: "defaults", set: map[string]any{
		"project_name": "app",
		"module":       "example.com/app",
		"go_version":   "1.22",
		"license":      "MIT",
		"with_git":     true,
		"features":     []any{},
	}}
}

// FlagSource 把命令行显式设置的选项包装为最高优先级来源。
// set 中只应包含用户显式给出的键。
func FlagSource(set map[string]any) Source {
	return &mapSource{name: "flags", set: set}
}

// FileSource 从 JSON 文件加载配置；字段名未知时由 Load 统一拒绝。
type FileSource struct {
	Path string
}

func (s *FileSource) Name() string { return "file:" + s.Path }

func (s *FileSource) Load() (*Layer, error) {
	data, err := os.ReadFile(s.Path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}
	// 先解码到 map 保留显式设置的键；json.Number 避免数值精度差异。
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("parse config file: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("parse config file: trailing data after JSON object")
	}
	// 数值会保留为 json.Number，buildParams 的类型校验会将其
	// 统一拒绝为 invalid_value，而不是误报为来源读取失败。
	return &Layer{Set: m}, nil
}
