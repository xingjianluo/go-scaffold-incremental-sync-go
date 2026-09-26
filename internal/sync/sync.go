// Package sync 实现模板与既有项目之间的三方合并增量更新。
//
// 对每个模板文件比较三方哈希：
//   - base：上次生成时写入的内容（清单记录）
//   - ours：项目目录当前内容（用户可能改过）
//   - theirs：本次模板渲染结果
//
// 仅模板侧改动 → 更新；仅用户侧改动 → 保留；双方都改 → 按 Policy 处理。
package sync

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Policy 是冲突处理策略。
type Policy string

const (
	PolicyFail   Policy = "fail"   // 双方同时改动时报冲突错误
	PolicyOurs   Policy = "ours"   // 保留项目侧改动
	PolicyTheirs Policy = "theirs" // 采用模板侧改动
)

// Valid 报告策略取值是否合法。
func (p Policy) Valid() bool {
	return p == PolicyFail || p == PolicyOurs || p == PolicyTheirs
}

// Decision 描述对单个文件的最终处理结论，保证结果可解释。
type Decision struct {
	Path   string
	Action string // create / update / keep / skip / delete / conflict
	Reason string
}

// Result 是一次增量同步的结论汇总，Decisions 按路径字典序排列。
type Result struct {
	Decisions []Decision
}

// ConflictError 汇总所有双方同时改动的文件，可与其它错误区分。
type ConflictError struct {
	Paths []string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("conflict: both template and project changed: %s",
		strings.Join(e.Paths, ", "))
}

// Applier 依据清单与策略把模板渲染结果同步到项目目录。
type Applier struct {
	Policy Policy
}

// Apply 对单个目标目录执行增量同步。
// rendered 的键为相对路径（使用 / 分隔），值为渲染后的完整内容。
// 无论成功或冲突，返回的 Result 都记录了每个已判定文件的结论与依据。
func (a *Applier) Apply(projectDir string, rendered map[string]string) (*Result, error) {
	if !a.Policy.Valid() {
		return nil, fmt.Errorf("invalid conflict policy %q", a.Policy)
	}
	manifest, err := LoadManifest(projectDir)
	if err != nil {
		return nil, err
	}
	res := &Result{}
	var conflicts []string

	// 固定按路径字典序处理，保证结论顺序与写入顺序确定。
	paths := make([]string, 0, len(rendered))
	for p := range rendered {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, p := range paths {
		want := rendered[p]
		wantHash := Hash(want)
		baseHash, hadBase := manifest.Files[p]
		full := filepath.Join(projectDir, filepath.FromSlash(p))
		curBytes, readErr := os.ReadFile(full)
		exists := readErr == nil
		if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
			return res, fmt.Errorf("read %s: %w", p, readErr)
		}

		switch {
		case !exists && !hadBase:
			// 新文件：直接创建。
			if err := writeFile(full, want); err != nil {
				return res, err
			}
			manifest.Files[p] = wantHash
			res.Decisions = append(res.Decisions, Decision{p, "create", "new template file"})
		case !exists && hadBase:
			// 用户删除了生成过的文件：尊重删除，不再重建。
			delete(manifest.Files, p)
			res.Decisions = append(res.Decisions, Decision{p, "skip", "deleted by user, keeping deletion"})
		case exists:
			curHash := Hash(string(curBytes))
			userChanged := curHash != baseHash
			templateChanged := wantHash != baseHash
			switch {
			case curHash == wantHash:
				manifest.Files[p] = wantHash
				res.Decisions = append(res.Decisions, Decision{p, "keep", "already up to date"})
			case !userChanged && templateChanged:
				// 仅模板演进：安全更新。
				if err := writeFile(full, want); err != nil {
					return res, err
				}
				manifest.Files[p] = wantHash
				res.Decisions = append(res.Decisions, Decision{p, "update", "template changed, project untouched"})
			case userChanged && !templateChanged:
				// 仅用户改动：保留。
				manifest.Files[p] = wantHash
				res.Decisions = append(res.Decisions, Decision{p, "keep", "project changed, template unchanged"})
			default:
				// 双方都改：按策略处理。
				switch a.Policy {
				case PolicyFail:
					conflicts = append(conflicts, p)
					res.Decisions = append(res.Decisions, Decision{p, "conflict",
						"both template and project changed"})
				case PolicyOurs:
					manifest.Files[p] = wantHash
					res.Decisions = append(res.Decisions, Decision{p, "keep",
						"both changed, policy=ours keeps project version"})
				case PolicyTheirs:
					if err := writeFile(full, want); err != nil {
						return res, err
					}
					manifest.Files[p] = wantHash
					res.Decisions = append(res.Decisions, Decision{p, "update",
						"both changed, policy=theirs applies template version"})
				}
			}
		}
	}

	// 模板已移除、且用户未改动的文件：删除；用户改过的保留。
	stale := make([]string, 0)
	for p := range manifest.Files {
		if _, ok := rendered[p]; !ok {
			stale = append(stale, p)
		}
	}
	sort.Strings(stale)
	for _, p := range stale {
		full := filepath.Join(projectDir, filepath.FromSlash(p))
		curBytes, readErr := os.ReadFile(full)
		if errors.Is(readErr, fs.ErrNotExist) {
			delete(manifest.Files, p)
			res.Decisions = append(res.Decisions, Decision{p, "skip", "removed from template, already absent"})
			continue
		}
		if readErr != nil {
			return res, fmt.Errorf("read %s: %w", p, readErr)
		}
		if Hash(string(curBytes)) == manifest.Files[p] {
			if err := os.Remove(full); err != nil {
				return res, fmt.Errorf("remove %s: %w", p, err)
			}
			delete(manifest.Files, p)
			res.Decisions = append(res.Decisions, Decision{p, "delete", "removed from template, project untouched"})
		} else {
			delete(manifest.Files, p)
			res.Decisions = append(res.Decisions, Decision{p, "keep", "removed from template but modified by user"})
		}
	}

	if len(conflicts) > 0 {
		// 冲突时不落盘清单，避免把未解决的中间态固化为 base。
		return res, &ConflictError{Paths: conflicts}
	}
	if err := manifest.Save(projectDir); err != nil {
		return res, err
	}
	return res, nil
}

// writeFile 先写临时文件再原子改名，避免写一半留下损坏内容。
func writeFile(full, content string) error {
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("create dir for %s: %w", full, err)
	}
	tmp := full + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", full, err)
	}
	if err := os.Rename(tmp, full); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("commit %s: %w", full, err)
	}
	return nil
}
