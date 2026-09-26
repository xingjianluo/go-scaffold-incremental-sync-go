package config

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
)

// DecodeFile 从 JSON 文件解码一层参数。
// 未知字段被拒绝（KindUnknownField），语法错误归为 KindInvalidValue，
// 读取失败归为 KindIO。层名取文件路径。
func DecodeFile(path string) (Layer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Layer{}, newErr(KindIO, path, "", "read: %v", err)
	}
	return Decode(path, data)
}

// Decode 从 JSON 字节解码一层参数，source 为层名。
func Decode(source string, data []byte) (Layer, error) {
	var p Params
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		kind := KindInvalidValue
		if strings.Contains(err.Error(), "unknown field") {
			kind = KindUnknownField
		}
		return Layer{}, newErr(kind, source, "", "decode: %v", err)
	}
	// 再解一次以确定哪些字段被显式设置。
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return Layer{}, newErr(KindInvalidValue, source, "", "decode: %v", err)
	}
	set := map[string]bool{}
	for k := range raw {
		set[k] = true
	}
	return Layer{Name: source, Params: p, Set: set}, nil
}

// FromEnv 从环境变量构造一层。prefix 如 "SCAFFOLD"，读取：
//
//	<prefix>_PROJECT_NAME / _MODULE_PATH / _GO_VERSION / _LICENSE /
//	<prefix>_FEATURES（逗号分隔）/ <prefix>_LABELS（k=v,k=v）
//
// 只把真实存在的环境变量记入 Set。
func FromEnv(prefix string, lookup func(string) (string, bool)) Layer {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	l := Layer{Name: "env:" + prefix, Set: map[string]bool{}}
	if v, ok := lookup(prefix + "_PROJECT_NAME"); ok {
		l.Params.ProjectName, l.Set["projectName"] = v, true
	}
	if v, ok := lookup(prefix + "_MODULE_PATH"); ok {
		l.Params.ModulePath, l.Set["modulePath"] = v, true
	}
	if v, ok := lookup(prefix + "_GO_VERSION"); ok {
		l.Params.GoVersion, l.Set["goVersion"] = v, true
	}
	if v, ok := lookup(prefix + "_LICENSE"); ok {
		l.Params.License, l.Set["license"] = v, true
	}
	if v, ok := lookup(prefix + "_FEATURES"); ok {
		l.Params.Features = splitCSV(v)
		l.Set["features"] = true
	}
	if v, ok := lookup(prefix + "_LABELS"); ok {
		l.Params.Labels = map[string]string{}
		for _, kv := range splitCSV(v) {
			k, val, _ := strings.Cut(kv, "=")
			l.Params.Labels[k] = val
		}
		l.Set["labels"] = true
	}
	return l
}

// FromFlags 基于 flag.FlagSet 构造一层，只有命令行上真实出现的
// 标志（flag.Visit）才记入 Set，保证默认值不会误覆盖低层配置。
func FromFlags(fs *flag.FlagSet) Layer {
	l := Layer{Name: "flags", Set: map[string]bool{}}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "name":
			l.Params.ProjectName, l.Set["projectName"] = f.Value.String(), true
		case "module":
			l.Params.ModulePath, l.Set["modulePath"] = f.Value.String(), true
		case "go":
			l.Params.GoVersion, l.Set["goVersion"] = f.Value.String(), true
		case "license":
			l.Params.License, l.Set["license"] = f.Value.String(), true
		case "features":
			l.Params.Features = splitCSV(f.Value.String())
			l.Set["features"] = true
		case "labels":
			l.Params.Labels = map[string]string{}
			for _, kv := range splitCSV(f.Value.String()) {
				k, val, _ := strings.Cut(kv, "=")
				l.Params.Labels[k] = val
			}
			l.Set["labels"] = true
		}
	})
	return l
}

// NewFlagSet 注册与 Params 对应的全部标志。
func NewFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.String("name", "", "project name")
	fs.String("module", "", "module path")
	fs.String("go", "", "go version")
	fs.String("license", "", "license id")
	fs.String("features", "", "comma separated features")
	fs.String("labels", "", "comma separated k=v labels")
	return fs
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		out = append(out, strings.TrimSpace(p))
	}
	return out
}

// String 以确定顺序输出参数，便于日志与调试（map 按键排序）。
func String(p Params) string {
	var b strings.Builder
	fmt.Fprintf(&b, "projectName=%s modulePath=%s goVersion=%s license=%s features=%v labels={",
		p.ProjectName, p.ModulePath, p.GoVersion, p.License, p.Features)
	for i, k := range sortedStringKeys(p.Labels) {
		if i > 0 {
			b.WriteString(" ")
		}
		fmt.Fprintf(&b, "%s:%s", k, p.Labels[k])
	}
	b.WriteString("}")
	return b.String()
}
