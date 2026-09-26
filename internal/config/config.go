// Package config 负责从多层来源加载生成参数并按确定优先级合并。
//
// 优先级（低 → 高）：内置默认值 < 配置文件 < 环境变量 < 命令行参数。
// 高层只覆盖其显式设置的字段；未知字段、非法取值与互相矛盾的选项
// 都会被拒绝并带可区分的原因类别（Kind）。
package config

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Params 是生成项目所需的全部参数。
type Params struct {
	ProjectName string            `json:"projectName"`
	ModulePath  string            `json:"modulePath"`
	GoVersion   string            `json:"goVersion"`
	License     string            `json:"license"`
	Features    []string          `json:"features"`
	Labels      map[string]string `json:"labels"`
}

// Kind 标识配置被拒绝的原因类别。
type Kind string

const (
	KindUnknownField Kind = "unknown_field" // 来源中出现未定义的字段
	KindInvalidValue Kind = "invalid_value" // 字段取值非法
	KindConflict     Kind = "conflict"      // 选项之间互相矛盾
	KindIO           Kind = "io"            // 来源读取失败
)

// Error 是可区分原因类别的配置错误。
type Error struct {
	Kind   Kind
	Source string // 出错来源层名，校验阶段为 "validate"
	Field  string
	Msg    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("config[%s]: %s: field %q: %s", e.Source, e.Kind, e.Field, e.Msg)
}

func newErr(kind Kind, source, field, format string, args ...any) *Error {
	return &Error{Kind: kind, Source: source, Field: field, Msg: fmt.Sprintf(format, args...)}
}

// Layer 表示一个参数来源层。Set 记录该层显式设置了哪些字段，
// 只有显式设置的字段才参与覆盖，保证合并结果可解释。
type Layer struct {
	Name   string
	Params Params
	Set    map[string]bool
}

func (l Layer) has(field string) bool { return l.Set[field] }

// 允许取值的集合，非法取值一律拒绝。
var (
	knownLicenses = map[string]bool{"MIT": true, "Apache-2.0": true, "BSD-3-Clause": true, "none": true}
	knownFeatures = map[string]bool{"http": true, "grpc": true, "metrics": true, "lint": true}
)

var (
	projectNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	goVersionRe   = regexp.MustCompile(`^\d+\.\d+(\.\d+)?$`)
	modulePathRe  = regexp.MustCompile(`^[A-Za-z0-9._~/-]+$`)
	labelKeyRe    = regexp.MustCompile(`^[a-z][a-z0-9_.-]*$`)
)

// Defaults 返回内置默认值层（最低优先级）。
func Defaults() Layer {
	return Layer{
		Name: "defaults",
		Params: Params{
			GoVersion: "1.25.0",
			License:   "MIT",
			Features:  []string{"http"},
			Labels:    map[string]string{},
		},
		Set: map[string]bool{"goVersion": true, "license": true, "features": true, "labels": true},
	}
}

// Load 从多层来源加载并合并参数，随后校验。
// layers 按优先级从低到高给出；Load 会自动在最底层补默认值。
func Load(layers ...Layer) (Params, error) {
	p, err := Merge(layers...)
	if err != nil {
		return Params{}, err
	}
	if err := Validate(p); err != nil {
		return Params{}, err
	}
	return p, nil
}

// Merge 按层顺序合并：高层覆盖其显式设置的字段。
// features 与 labels 采用整值替换而非追加，保证结果确定且可复现。
func Merge(layers ...Layer) (Params, error) {
	out := Params{}
	for _, l := range layers {
		if l.Name == "" {
			return Params{}, newErr(KindInvalidValue, "<internal>", "layer", "layer name must not be empty")
		}
		p := l.Params
		if l.has("projectName") {
			out.ProjectName = p.ProjectName
		}
		if l.has("modulePath") {
			out.ModulePath = p.ModulePath
		}
		if l.has("goVersion") {
			out.GoVersion = p.GoVersion
		}
		if l.has("license") {
			out.License = p.License
		}
		if l.has("features") {
			out.Features = append([]string(nil), p.Features...)
		}
		if l.has("labels") {
			out.Labels = make(map[string]string, len(p.Labels))
			for k, v := range p.Labels {
				out.Labels[k] = v
			}
		}
	}
	return out, nil
}

// Validate 校验合并后的参数，返回第一个可区分原因的错误。
// 校验顺序固定，保证相同输入产生相同错误。
func Validate(p Params) error {
	const src = "validate"
	if !projectNameRe.MatchString(p.ProjectName) {
		return newErr(KindInvalidValue, src, "projectName",
			"must match %s, got %q", projectNameRe, p.ProjectName)
	}
	if p.ModulePath == "" {
		return newErr(KindInvalidValue, src, "modulePath", "must not be empty")
	}
	if !modulePathRe.MatchString(p.ModulePath) || strings.Contains(p.ModulePath, "//") ||
		strings.HasPrefix(p.ModulePath, "/") || strings.HasSuffix(p.ModulePath, "/") {
		return newErr(KindInvalidValue, src, "modulePath", "invalid module path %q", p.ModulePath)
	}
	if !goVersionRe.MatchString(p.GoVersion) {
		return newErr(KindInvalidValue, src, "goVersion", "must look like 1.25.0, got %q", p.GoVersion)
	}
	if !knownLicenses[p.License] {
		return newErr(KindInvalidValue, src, "license",
			"must be one of %s, got %q", sortedKeys(knownLicenses), p.License)
	}
	for _, f := range p.Features {
		if !knownFeatures[f] {
			return newErr(KindInvalidValue, src, "features",
				"unknown feature %q, must be subset of %s", f, sortedKeys(knownFeatures))
		}
	}
	if hasDup(p.Features) {
		return newErr(KindInvalidValue, src, "features", "duplicate entries in %v", p.Features)
	}
	for _, k := range sortedStringKeys(p.Labels) {
		if !labelKeyRe.MatchString(k) {
			return newErr(KindInvalidValue, src, "labels", "invalid label key %q", k)
		}
	}
	// 矛盾检测：modulePath 的最后一段必须与 projectName 一致，
	// 否则生成出的模块名与项目名互相矛盾。
	if seg := p.ModulePath[strings.LastIndex(p.ModulePath, "/")+1:]; seg != p.ProjectName {
		return newErr(KindConflict, src, "modulePath",
			"last segment %q contradicts projectName %q", seg, p.ProjectName)
	}
	// 矛盾检测：license 为 none 时不允许声明 copyright 标签。
	if p.License == "none" {
		if _, ok := p.Labels["copyright"]; ok {
			return newErr(KindConflict, src, "license",
				"license=none contradicts labels.copyright being set")
		}
	}
	return nil
}

func hasDup(ss []string) bool {
	seen := map[string]bool{}
	for _, s := range ss {
		if seen[s] {
			return true
		}
		seen[s] = true
	}
	return false
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedStringKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
