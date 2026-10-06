// Package sync 实现模板与既有项目之间的三方合并增量更新。
//
// 对每个模板文件比较三方哈希：
//   - base：上次生成时写入的内容（清单记录）
//   - ours：项目目录当前内容（用户可能改过）
//   - theirs：本次模板渲染结果
//
// 仅模板侧改动 → 更新；仅用户侧改动 → 保留；双方都改 → 按 Policy 处理。
//
// Plan 与 Apply 共享同一份纯只读评估（evaluate）：Plan 只输出结论不动盘，
// Apply 按评估出的变更序列落盘，因此预演结论与实际执行严格一致。
package sync

import (
	"context"
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
	Action string // create / update / keep / skip / delete / conflict / untouched
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

	// beforeWrite 在每个文件变更落盘前回调（相对路径），仅供测试确定性地
	// 在写入中途触发取消；生产代码不设置。
	beforeWrite func(rel string)
}

// mutation 是评估阶段算出的、Apply 阶段按序执行的单个落盘变更。
type mutation struct {
	path    string // 相对路径（/ 分隔）
	kind    string // "write" | "delete"
	content string // kind == "write" 时的目标内容
}

// Plan 只做预演：返回与 Apply 完全相同的判定结论，但不创建、修改或删除
// 项目中的任何文件，也不写清单。存在冲突（PolicyFail）时返回 *ConflictError，
// 但 Result 仍包含全部文件的判定，供调用方展示待决冲突。
func (a *Applier) Plan(projectDir string, rendered map[string]string) (*Result, error) {
	if !a.Policy.Valid() {
		return nil, fmt.Errorf("invalid conflict policy %q", a.Policy)
	}
	res, _, _, conflicts, err := a.evaluate(projectDir, rendered)
	if err != nil {
		return res, err
	}
	if len(conflicts) > 0 {
		return res, &ConflictError{Paths: conflicts}
	}
	return res, nil
}

// Apply 对单个目标目录执行增量同步。
// rendered 的键为相对路径（使用 / 分隔），值为渲染后的完整内容。
// 无论成功或冲突，返回的 Result 都记录了每个已判定文件的结论与依据。
//
// ctx 被取消或超时时：不会再开始任何新的写入/删除，已完成的原子改名
// 不受影响，清单不会提交半成品；错误原样向上返回（可 errors.Is 判定）。
// 取消后重新 Apply 是安全的：残留临时文件会被清理，三方合并天然收敛。
func (a *Applier) Apply(ctx context.Context, projectDir string, rendered map[string]string) (*Result, error) {
	if !a.Policy.Valid() {
		return nil, fmt.Errorf("invalid conflict policy %q", a.Policy)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	res, mutations, nextManifest, conflicts, err := a.evaluate(projectDir, rendered)
	if err != nil {
		return res, err
	}
	if len(conflicts) > 0 {
		// 冲突时不落盘清单，避免把未解决的中间态固化为 base。
		return res, &ConflictError{Paths: conflicts}
	}

	// 清理上次被中断可能留下的临时文件（原子改名前的那一份），
	// 保证取消/崩溃后重新执行不会残留清不掉的临时文件。
	if err := sweepTempFiles(projectDir, rendered); err != nil {
		return res, err
	}

	for _, m := range mutations {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		full := filepath.Join(projectDir, filepath.FromSlash(m.path))
		switch m.kind {
		case "write":
			if a.beforeWrite != nil {
				a.beforeWrite(m.path)
			}
			if err := ctx.Err(); err != nil {
				return res, err
			}
			if err := writeFile(full, m.content); err != nil {
				return res, err
			}
		case "delete":
			if a.beforeWrite != nil {
				a.beforeWrite(m.path)
			}
			if err := ctx.Err(); err != nil {
				return res, err
			}
			if err := os.Remove(full); err != nil {
				return res, fmt.Errorf("remove %s: %w", m.path, err)
			}
		}
	}

	if err := ctx.Err(); err != nil {
		return res, err
	}
	if err := nextManifest.Save(projectDir); err != nil {
		return res, err
	}
	return res, nil
}

// evaluate 是 Plan 与 Apply 共用的纯只读判定：只读项目目录与清单，
// 产出按路径字典序排列的结论、待执行变更序列与提交后的清单状态。
// 它本身不修改任何文件。
func (a *Applier) evaluate(projectDir string, rendered map[string]string) (
	*Result, []mutation, *Manifest, []string, error,
) {
	manifest, err := LoadManifest(projectDir)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	next := &Manifest{Files: make(map[string]string, len(manifest.Files))}
	for k, v := range manifest.Files {
		next.Files[k] = v
	}
	res := &Result{}
	var conflicts []string
	var mutations []mutation

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
			return res, nil, nil, nil, fmt.Errorf("read %s: %w", p, readErr)
		}

		switch {
		case !exists && !hadBase:
			// 新文件：直接创建。
			mutations = append(mutations, mutation{path: p, kind: "write", content: want})
			next.Files[p] = wantHash
			res.Decisions = append(res.Decisions, Decision{p, "create", "new template file"})
		case !exists && hadBase:
			// 用户删除了生成过的文件：尊重删除，不再重建。
			delete(next.Files, p)
			res.Decisions = append(res.Decisions, Decision{p, "skip", "deleted by user, keeping deletion"})
		case exists:
			curHash := Hash(string(curBytes))
			userChanged := curHash != baseHash
			templateChanged := wantHash != baseHash
			switch {
			case curHash == wantHash:
				next.Files[p] = wantHash
				res.Decisions = append(res.Decisions, Decision{p, "keep", "already up to date"})
			case !userChanged && templateChanged:
				// 仅模板演进：安全更新。
				mutations = append(mutations, mutation{path: p, kind: "write", content: want})
				next.Files[p] = wantHash
				res.Decisions = append(res.Decisions, Decision{p, "update", "template changed, project untouched"})
			case userChanged && !templateChanged:
				// 仅用户改动：保留。
				next.Files[p] = wantHash
				res.Decisions = append(res.Decisions, Decision{p, "keep", "project changed, template unchanged"})
			default:
				// 双方都改：按策略处理。
				switch a.Policy {
				case PolicyFail:
					conflicts = append(conflicts, p)
					res.Decisions = append(res.Decisions, Decision{p, "conflict",
						"both template and project changed"})
				case PolicyOurs:
					next.Files[p] = wantHash
					res.Decisions = append(res.Decisions, Decision{p, "keep",
						"both changed, policy=ours keeps project version"})
				case PolicyTheirs:
					mutations = append(mutations, mutation{path: p, kind: "write", content: want})
					next.Files[p] = wantHash
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
			delete(next.Files, p)
			res.Decisions = append(res.Decisions, Decision{p, "skip", "removed from template, already absent"})
			continue
		}
		if readErr != nil {
			return res, nil, nil, nil, fmt.Errorf("read %s: %w", p, readErr)
		}
		if Hash(string(curBytes)) == manifest.Files[p] {
			mutations = append(mutations, mutation{path: p, kind: "delete"})
			delete(next.Files, p)
			res.Decisions = append(res.Decisions, Decision{p, "delete", "removed from template, project untouched"})
		} else {
			delete(next.Files, p)
			res.Decisions = append(res.Decisions, Decision{p, "keep", "removed from template but modified by user"})
		}
	}

	// 本地存在、但模板与清单都不接管的文件：原样保留，永不触碰。
	if err := collectUntouched(projectDir, rendered, manifest, res); err != nil {
		return res, nil, nil, nil, err
	}

	sort.SliceStable(res.Decisions, func(i, j int) bool {
		return res.Decisions[i].Path < res.Decisions[j].Path
	})
	sort.Strings(conflicts)
	return res, mutations, next, conflicts, nil
}

// collectUntouched 遍历项目目录，把未被模板与清单接管的本地文件记为 untouched。
// 目标目录尚不存在时（首次生成）没有本地文件，直接跳过。
func collectUntouched(projectDir string, rendered map[string]string, manifest *Manifest, res *Result) error {
	if _, err := os.Stat(projectDir); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("stat %s: %w", projectDir, err)
	}
	return filepath.WalkDir(projectDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(projectDir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == ManifestPath {
			return nil
		}
		if _, ok := rendered[rel]; ok {
			return nil
		}
		if _, ok := manifest.Files[rel]; ok {
			return nil
		}
		res.Decisions = append(res.Decisions, Decision{rel, "untouched",
			"not managed by template, kept as-is"})
		return nil
	})
}

// sweepTempFiles 删除已知临时位置可能残留的 .tmp 文件（上次执行在原子改名
// 前被取消/崩溃）。只处理本工具自己的临时文件，不碰用户文件。
func sweepTempFiles(projectDir string, rendered map[string]string) error {
	candidates := make([]string, 0, len(rendered)+1)
	for p := range rendered {
		candidates = append(candidates, filepath.Join(projectDir, filepath.FromSlash(p)+".tmp"))
	}
	candidates = append(candidates, filepath.Join(projectDir, ManifestPath+".tmp"))
	sort.Strings(candidates)
	for _, tmp := range candidates {
		if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("clean temp %s: %w", tmp, err)
		}
	}
	return nil
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
