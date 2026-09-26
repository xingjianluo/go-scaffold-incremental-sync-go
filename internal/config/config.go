// Package config 负责从多层来源加载生成参数并按确定优先级合并。
// 合并优先级固定为：先注册的来源优先级低，后注册的来源覆盖先注册的。
// 未知字段、非法取值与互相矛盾的选项都会以可区分的 *Error 拒绝。
package config

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Kind 区分配置错误的类别，便于调用方区分拒绝原因。
type Kind string

const (
	KindUnknownField  Kind = "unknown_field"  // 出现未定义的字段
	KindInvalidValue  Kind = "invalid_value"  // 字段取值非法
	KindConflict      Kind = "conflict"       // 选项之间互相矛盾
	KindSourceFailure Kind = "source_failure" // 配置来源读取失败
)

// Error 是可区分原因的配置错误。
type Error struct {
	Kind   Kind
	Field  string
	Reason string
}

func (e *Error) Error() string {
	return fmt.Sprintf("config %s: field %q: %s", e.Kind, e.Field, e.Reason)
}

// Params 是合并后的生成参数。
type Params struct {
	ProjectName string
	Module      string
	GoVersion   string
	License     string
	WithGit     bool
	Features    []string
}

// Source 是一个配置来源，返回该来源显式设置的字段。
type Source interface {
	Name() string
	Load() (*Layer, error)
}

// Layer 记录某一来源显式设置的字段值。
type Layer struct {
	Set map[string]any
}

// knownFields 是允许出现的字段全集，按字典序排列以保证遍历确定。
var knownFields = []string{
	"features",
	"go_version",
	"license",
	"module",
	"project_name",
	"with_git",
}

var (
	modulePattern    = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._~/-]*$`)
	goVersionPattern = regexp.MustCompile(`^\d+\.\d+(\.\d+)?$`)
	allowedLicenses  = map[string]bool{"MIT": true, "Apache-2.0": true, "BSD-3-Clause": true}
)

// Load 按来源注册顺序（后者覆盖前者）合并所有来源并严格校验。
func Load(sources ...Source) (*Params, error) {
	merged := map[string]any{}
	for _, src := range sources {
		layer, err := src.Load()
		if err != nil {
			return nil, &Error{Kind: KindSourceFailure, Field: src.Name(), Reason: err.Error()}
		}
		if layer == nil {
			continue
		}
		// 先校验该来源的字段名，未知字段立即拒绝。
		keys := make([]string, 0, len(layer.Set))
		for k := range layer.Set {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if !isKnown(k) {
				return nil, &Error{Kind: KindUnknownField, Field: k,
					Reason: fmt.Sprintf("source %q sets undefined field", src.Name())}
			}
			merged[k] = layer.Set[k]
		}
	}
	p, err := buildParams(merged)
	if err != nil {
		return nil, err
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}

func isKnown(field string) bool {
	i := sort.SearchStrings(knownFields, field)
	return i < len(knownFields) && knownFields[i] == field
}

// buildParams 把合并后的 map 转成强类型 Params，类型不符即拒绝。
func buildParams(m map[string]any) (*Params, error) {
	p := &Params{}
	for _, k := range knownFields { // 固定顺序，保证错误判定确定
		v, ok := m[k]
		if !ok {
			continue
		}
		switch k {
		case "project_name":
			s, ok := v.(string)
			if !ok {
				return nil, &Error{Kind: KindInvalidValue, Field: k, Reason: "must be a string"}
			}
			p.ProjectName = s
		case "module":
			s, ok := v.(string)
			if !ok {
				return nil, &Error{Kind: KindInvalidValue, Field: k, Reason: "must be a string"}
			}
			p.Module = s
		case "go_version":
			s, ok := v.(string)
			if !ok {
				return nil, &Error{Kind: KindInvalidValue, Field: k, Reason: "must be a string"}
			}
			p.GoVersion = s
		case "license":
			s, ok := v.(string)
			if !ok {
				return nil, &Error{Kind: KindInvalidValue, Field: k, Reason: "must be a string"}
			}
			p.License = s
		case "with_git":
			b, ok := v.(bool)
			if !ok {
				return nil, &Error{Kind: KindInvalidValue, Field: k, Reason: "must be a bool"}
			}
			p.WithGit = b
		case "features":
			list, ok := v.([]any)
			if !ok {
				return nil, &Error{Kind: KindInvalidValue, Field: k, Reason: "must be an array of strings"}
			}
			feats := make([]string, 0, len(list))
			for i, item := range list {
				s, ok := item.(string)
				if !ok {
					return nil, &Error{Kind: KindInvalidValue, Field: k,
						Reason: fmt.Sprintf("element %d must be a string", i)}
				}
				feats = append(feats, s)
			}
			sort.Strings(feats) // 排序保证输出与输入顺序无关
			p.Features = feats
		}
	}
	return p, nil
}

// Validate 校验取值合法性与选项间一致性。
func (p *Params) Validate() error {
	if p.ProjectName == "" {
		return &Error{Kind: KindInvalidValue, Field: "project_name", Reason: "must not be empty"}
	}
	if p.Module == "" {
		return &Error{Kind: KindInvalidValue, Field: "module", Reason: "must not be empty"}
	}
	if !modulePattern.MatchString(p.Module) {
		return &Error{Kind: KindInvalidValue, Field: "module",
			Reason: "contains characters not allowed in a module path"}
	}
	if p.GoVersion == "" {
		return &Error{Kind: KindInvalidValue, Field: "go_version", Reason: "must not be empty"}
	}
	if !goVersionPattern.MatchString(p.GoVersion) {
		return &Error{Kind: KindInvalidValue, Field: "go_version",
			Reason: "must look like 1.22 or 1.22.3"}
	}
	if p.License != "" && !allowedLicenses[p.License] {
		return &Error{Kind: KindInvalidValue, Field: "license",
			Reason: fmt.Sprintf("must be one of %s", strings.Join(allowedLicenseList(), ", "))}
	}
	// 矛盾检测：声明了 git 特性却关闭 with_git，或反之缺失时的组合冲突。
	hasGitFeature := false
	for _, f := range p.Features {
		if f == "git" {
			hasGitFeature = true
		}
	}
	if hasGitFeature && !p.WithGit {
		return &Error{Kind: KindConflict, Field: "features",
			Reason: "feature \"git\" requires with_git=true, but with_git is false"}
	}
	return nil
}

func allowedLicenseList() []string {
	out := make([]string, 0, len(allowedLicenses))
	for l := range allowedLicenses {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}
