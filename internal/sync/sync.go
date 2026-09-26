// Package sync 实现已存在项目的增量同步与三方合并。
//
// 目录约定：目标目录下 .scaffold/manifest.json 记录每个被模板接管
// 文件的基线哈希，.scaffold/base/ 保存基线内容副本。同步时以
// base / 本地当前内容 / 模板新内容做三方比较：
//
//   - 本地未改 → 模板更新直接生效（update）
//   - 模板未改 → 本地修改原样保留（keep）
//   - 双方都改且不一致 → 按 Policy 处理，绝不静默覆盖
//
// Apply 分规划与执行两阶段：PolicyFail 下若存在冲突，
// 在执行前返回 ConflictError，不写入任何文件。
package sync

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/xingjianluo/go-scaffold-incremental-sync-go/internal/render"
)

// Policy 是冲突处理策略。
type Policy string

const (
	PolicyFail    Policy = "fail"    // 冲突即报错，不写入任何文件
	PolicyTheirs  Policy = "theirs"  // 以模板侧为准
	PolicyOurs    Policy = "ours"    // 以本地侧为准
	PolicyMarkers Policy = "markers" // 三方合并并写入冲突标记
)

// Policies 列出全部合法策略。
var Policies = []Policy{PolicyFail, PolicyTheirs, PolicyOurs, PolicyMarkers}

// Action 描述对单个文件的处理结论。
type Action struct {
	Path   string
	Kind   string // create / update / keep / conflict-* / deleted / stale
	Detail string
}

// Report 是一次同步的可解释结论，动作按路径排序。
type Report struct {
	Actions []Action
}

// HasConflicts 报告本次同步是否发生过冲突。
func (r *Report) HasConflicts() bool {
	for _, a := range r.Actions {
		if strings.HasPrefix(a.Kind, "conflict") {
			return true
		}
	}
	return false
}

// ConflictError 在 PolicyFail 且存在冲突时返回，列出全部冲突路径。
type ConflictError struct {
	Paths []string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("sync: %d conflict(s): %s", len(e.Paths), strings.Join(e.Paths, ", "))
}

const (
	manifestRel = ".scaffold/manifest.json"
	baseDirRel  = ".scaffold/base"
)

type manifestEntry struct {
	SHA256 string `json:"sha256"`
	Mode   uint32 `json:"mode"`
}

type manifest struct {
	Version int                      `json:"version"`
	Files   map[string]manifestEntry `json:"files"`
}

// Syncer 对目标目录执行增量同步。
type Syncer struct {
	Dir    string
	Policy Policy
}

type plan struct {
	action  Action
	write   *render.File // 非空表示需要写入的内容
	newBase []byte       // 写入后记录的新基线内容
	mode    uint32
	managed bool // 是否记入清单
}

// Apply 将渲染结果应用到目标目录，返回可解释的报告。
func (s *Syncer) Apply(files []render.File) (*Report, error) {
	switch s.Policy {
	case PolicyFail, PolicyTheirs, PolicyOurs, PolicyMarkers:
	default:
		return nil, fmt.Errorf("sync: unknown policy %q", s.Policy)
	}
	sorted := append([]render.File(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })

	mf, err := s.loadManifest()
	if err != nil {
		return nil, err
	}

	// 规划阶段：不触碰磁盘。
	plans := make([]plan, 0, len(sorted))
	var conflicts []string
	for _, f := range sorted {
		p, err := s.planOne(f, mf)
		if err != nil {
			return nil, err
		}
		if p.action.Kind == "conflict" {
			conflicts = append(conflicts, f.Path)
		}
		plans = append(plans, p)
	}
	if s.Policy == PolicyFail && len(conflicts) > 0 {
		return nil, &ConflictError{Paths: conflicts}
	}

	// 执行阶段：逐文件原子写入，最后写清单。
	report := &Report{}
	for i, p := range plans {
		if p.write != nil {
			if err := s.writeAtomic(p.write.Path, p.write.Content, p.mode); err != nil {
				return nil, err
			}
		}
		if p.managed {
			if err := s.writeBase(sorted[i].Path, p.newBase); err != nil {
				return nil, err
			}
			mf.Files[sorted[i].Path] = manifestEntry{SHA256: hash(p.newBase), Mode: p.mode}
		}
		report.Actions = append(report.Actions, p.action)
	}
	// 模板已删除但曾被接管的文件：保留本地内容，仅报告。
	for path := range mf.Files {
		found := false
		for _, f := range sorted {
			if f.Path == path {
				found = true
				break
			}
		}
		if !found {
			report.Actions = append(report.Actions, Action{Path: path, Kind: "stale",
				Detail: "no longer in template; local file kept"})
		}
	}
	sort.Slice(report.Actions, func(i, j int) bool { return report.Actions[i].Path < report.Actions[j].Path })

	if err := s.saveManifest(mf); err != nil {
		return nil, err
	}
	return report, nil
}

// planOne 计算单个文件的处理计划。
func (s *Syncer) planOne(f render.File, mf *manifest) (plan, error) {
	cur, curErr := os.ReadFile(filepath.Join(s.Dir, filepath.FromSlash(f.Path)))
	curExists := curErr == nil
	base, baseErr := os.ReadFile(s.basePath(f.Path))
	baseExists := baseErr == nil
	_, managed := mf.Files[f.Path]

	switch {
	case !managed && !curExists:
		return plan{
			action: Action{Path: f.Path, Kind: "create", Detail: "new file from template"},
			write:  &f, newBase: f.Content, mode: f.Mode, managed: true,
		}, nil
	case !managed && curExists:
		// 本地已存在同名文件但从未被接管：视为冲突，不静默覆盖。
		return s.resolveConflict(f, cur, nil, "untracked local file exists")
	case managed && !curExists:
		if s.Policy == PolicyTheirs {
			return plan{
				action: Action{Path: f.Path, Kind: "update", Detail: "locally deleted; restored by theirs policy"},
				write:  &f, newBase: f.Content, mode: f.Mode, managed: true,
			}, nil
		}
		return plan{
			action:  Action{Path: f.Path, Kind: "deleted", Detail: "locally deleted; deletion respected"},
			newBase: f.Content, mode: f.Mode, managed: true,
		}, nil
	case managed && curExists && !baseExists:
		return plan{}, fmt.Errorf("sync: manifest lists %s but base copy is missing", f.Path)
	}

	switch {
	case hash(cur) == hash(base) && hash(f.Content) == hash(base):
		return plan{action: Action{Path: f.Path, Kind: "keep", Detail: "unchanged"},
			newBase: f.Content, mode: f.Mode, managed: true}, nil
	case hash(cur) == hash(base):
		return plan{
			action: Action{Path: f.Path, Kind: "update", Detail: "template updated; local untouched"},
			write:  &f, newBase: f.Content, mode: f.Mode, managed: true,
		}, nil
	case hash(f.Content) == hash(base):
		return plan{action: Action{Path: f.Path, Kind: "keep", Detail: "local change preserved"},
			newBase: f.Content, mode: f.Mode, managed: true}, nil
	case hash(cur) == hash(f.Content):
		return plan{action: Action{Path: f.Path, Kind: "keep", Detail: "both sides converged"},
			newBase: f.Content, mode: f.Mode, managed: true}, nil
	default:
		return s.resolveConflict(f, cur, base, "both local and template changed")
	}
}

// resolveConflict 按策略生成冲突处理计划。
func (s *Syncer) resolveConflict(f render.File, cur, base []byte, why string) (plan, error) {
	switch s.Policy {
	case PolicyFail:
		return plan{action: Action{Path: f.Path, Kind: "conflict", Detail: why}}, nil
	case PolicyTheirs:
		return plan{
			action: Action{Path: f.Path, Kind: "conflict-theirs", Detail: why + "; template wins"},
			write:  &f, newBase: f.Content, mode: f.Mode, managed: true,
		}, nil
	case PolicyOurs:
		return plan{
			action:  Action{Path: f.Path, Kind: "conflict-ours", Detail: why + "; local kept"},
			newBase: f.Content, mode: f.Mode, managed: true,
		}, nil
	case PolicyMarkers:
		merged := Merge3(string(base), string(cur), string(f.Content))
		out := f
		out.Content = []byte(merged.String())
		return plan{
			action: Action{Path: f.Path, Kind: "conflict-markers",
				Detail: fmt.Sprintf("%s; merged with %d conflict block(s)", why, merged.Conflicts)},
			write: &out, newBase: f.Content, mode: f.Mode, managed: true,
		}, nil
	}
	return plan{}, fmt.Errorf("sync: unknown policy %q", s.Policy)
}

func (s *Syncer) loadManifest() (*manifest, error) {
	data, err := os.ReadFile(filepath.Join(s.Dir, filepath.FromSlash(manifestRel)))
	if os.IsNotExist(err) {
		return &manifest{Version: 1, Files: map[string]manifestEntry{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sync: read manifest: %w", err)
	}
	var mf manifest
	if err := json.Unmarshal(data, &mf); err != nil {
		return nil, fmt.Errorf("sync: parse manifest: %w", err)
	}
	if mf.Files == nil {
		mf.Files = map[string]manifestEntry{}
	}
	return &mf, nil
}

func (s *Syncer) saveManifest(mf *manifest) error {
	data, err := json.MarshalIndent(mf, "", "  ") // map 键由 encoding/json 排序，输出确定
	if err != nil {
		return err
	}
	return s.writeAtomic(manifestRel, append(data, '\n'), 0o644)
}

func (s *Syncer) basePath(rel string) string {
	return filepath.Join(s.Dir, filepath.FromSlash(baseDirRel), filepath.FromSlash(rel))
}

// writeBase 更新基线副本（记录模板侧内容，供下次三方比较）。
func (s *Syncer) writeBase(rel string, content []byte) error {
	p := s.basePath(rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, content, 0o644)
}

// writeAtomic 先写临时文件再 rename，避免半截文件。
func (s *Syncer) writeAtomic(rel string, content []byte, mode uint32) error {
	p := filepath.Join(s.Dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("sync: mkdir for %s: %w", rel, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".scaffold-tmp-*")
	if err != nil {
		return fmt.Errorf("sync: temp for %s: %w", rel, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("sync: write %s: %w", rel, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, os.FileMode(mode)); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, p); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("sync: rename %s: %w", rel, err)
	}
	return nil
}

func hash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
