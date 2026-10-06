// Package sync 实现模板与既有项目之间的三方合并增量更新。
//
// 对每个模板文件比较三方哈希：
//   - base：上次生成时写入的内容（清单记录）
//   - ours：项目目录当前内容（用户可能改过）
//   - theirs：本次模板渲染结果
//
// 仅模板侧改动 → 更新；仅用户侧改动 → 保留；双方都改 → 按 Policy 处理。
//
// 判定与落盘是分离的两步（见 analyze / ApplyContext），因此 Plan（预演，
// 只读）与 ApplyContext（真正写盘）对同一文件系统状态必然给出相同结论。
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

// fileAction 是 analyze 对单个路径的判定：对外呈现为 Decision，
// 对内还携带执行阶段需要的写/删动作与目标内容。
type fileAction struct {
	path    string
	op      string // create / update / keep / skip / delete / conflict
	reason  string
	write   bool   // 执行阶段需要写入 content
	delete  bool   // 执行阶段需要删除该文件
	content string // write=true 时的目标内容
}

// analysis 是只读分析的产物：按字典序排列的判定、成功后应落盘的清单，
// 以及冲突路径。它不持有任何需要释放的资源。
type analysis struct {
	actions   []fileAction
	next      *Manifest
	conflicts []string
}

func (an *analysis) result() *Result {
	res := &Result{Decisions: make([]Decision, 0, len(an.actions))}
	for _, ac := range an.actions {
		res.Decisions = append(res.Decisions, Decision{Path: ac.path, Action: ac.op, Reason: ac.reason})
	}
	return res
}

// Apply 对单个目标目录执行增量同步（使用 context.Background）。
// rendered 的键为相对路径（使用 / 分隔），值为渲染后的完整内容。
// 无论成功或冲突，返回的 Result 都记录了每个已判定文件的结论与依据。
func (a *Applier) Apply(projectDir string, rendered map[string]string) (*Result, error) {
	return a.ApplyContext(context.Background(), projectDir, rendered)
}

// Plan 只做只读预演：不创建、不修改、不删除任何文件，也不写清单。
// 在 policy=fail 且存在双方同时改动的文件时，返回的 Result 仍包含全部
// 判定（冲突文件的 Action 为 conflict），同时返回 *ConflictError。
// Plan 与 ApplyContext 共用 analyze，因此对同一文件系统状态，
// 预演结论与真正执行的逐文件结论完全一致。
func (a *Applier) Plan(ctx context.Context, projectDir string, rendered map[string]string) (*Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	an, err := a.analyze(projectDir, rendered)
	if err != nil {
		return nil, err
	}
	res := an.result()
	if len(an.conflicts) > 0 {
		return res, &ConflictError{Paths: an.conflicts}
	}
	return res, nil
}

// ApplyContext 对单个目标目录执行增量同步，并响应 ctx 取消。
//
// 取消语义：先完成全部只读分析；执行阶段在每个变更动作（写文件、删文件、
// 提交清单）之前检查 ctx，取消后不再发起新的变更，已开始的单个文件写入
// 借助“临时文件 + 原子改名”必然完整或不存在。清单最后一次性原子提交，
// 因此取消后重新执行是幂等收敛的，不会出现重复写入或计数错乱。
func (a *Applier) ApplyContext(ctx context.Context, projectDir string, rendered map[string]string) (*Result, error) {
	an, err := a.analyze(projectDir, rendered)
	if err != nil {
		return nil, err
	}
	res := an.result()

	// 按分析顺序执行变更；每个动作前检查取消。
	for _, ac := range an.actions {
		if !ac.write && !ac.delete {
			continue
		}
		if err := ctx.Err(); err != nil {
			return res, fmt.Errorf("apply cancelled before %s %s: %w", ac.op, ac.path, err)
		}
		full := filepath.Join(projectDir, filepath.FromSlash(ac.path))
		if ac.write {
			if err := writeFile(full, ac.content); err != nil {
				return res, err
			}
		} else {
			if err := os.Remove(full); err != nil {
				return res, fmt.Errorf("remove %s: %w", ac.path, err)
			}
		}
	}

	if len(an.conflicts) > 0 {
		// 冲突时不落盘清单，避免把未解决的中间态固化为 base。
		return res, &ConflictError{Paths: an.conflicts}
	}
	if err := ctx.Err(); err != nil {
		return res, fmt.Errorf("apply cancelled before manifest commit: %w", err)
	}
	if err := an.next.Save(projectDir); err != nil {
		return res, err
	}
	return res, nil
}

// analyze 执行全部只读的三方分析，不触碰任何文件内容与清单。
// 固定按路径字典序处理，保证结论顺序确定，且 Plan/ApplyContext 结论一致。
func (a *Applier) analyze(projectDir string, rendered map[string]string) (*analysis, error) {
	if !a.Policy.Valid() {
		return nil, fmt.Errorf("invalid conflict policy %q", a.Policy)
	}
	manifest, err := LoadManifest(projectDir)
	if err != nil {
		return nil, err
	}
	// 在副本上演算成功后的清单；只有全部动作完成后才一次性落盘。
	next := &Manifest{Files: make(map[string]string, len(manifest.Files))}
	for k, v := range manifest.Files {
		next.Files[k] = v
	}
	an := &analysis{next: next}

	paths := make([]string, 0, len(rendered))
	for p := range rendered {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, p := range paths {
		want := rendered[p]
		wantHash := Hash(want)
		_, hadBase := manifest.Files[p]
		full := filepath.Join(projectDir, filepath.FromSlash(p))
		curBytes, readErr := os.ReadFile(full)
		exists := readErr == nil
		if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
			return nil, fmt.Errorf("read %s: %w", p, readErr)
		}

		switch {
		case !exists && !hadBase:
			// 新文件：直接创建。
			next.Files[p] = wantHash
			an.actions = append(an.actions, fileAction{
				path: p, op: "create", reason: "new template file",
				write: true, content: want,
			})
		case !exists && hadBase:
			// 用户删除了生成过的文件：尊重删除，不再重建。
			delete(next.Files, p)
			an.actions = append(an.actions, fileAction{
				path: p, op: "skip", reason: "deleted by user, keeping deletion",
			})
		case exists:
			curHash := Hash(string(curBytes))
			baseHash := manifest.Files[p]
			userChanged := curHash != baseHash
			templateChanged := wantHash != baseHash
			switch {
			case curHash == wantHash:
				next.Files[p] = wantHash
				an.actions = append(an.actions, fileAction{
					path: p, op: "keep", reason: "already up to date",
				})
			case !userChanged && templateChanged:
				// 仅模板演进：安全更新。
				next.Files[p] = wantHash
				an.actions = append(an.actions, fileAction{
					path: p, op: "update", reason: "template changed, project untouched",
					write: true, content: want,
				})
			case userChanged && !templateChanged:
				// 仅用户改动：保留。
				next.Files[p] = wantHash
				an.actions = append(an.actions, fileAction{
					path: p, op: "keep", reason: "project changed, template unchanged",
				})
			default:
				// 双方都改：按策略处理。
				switch a.Policy {
				case PolicyFail:
					an.conflicts = append(an.conflicts, p)
					an.actions = append(an.actions, fileAction{
						path: p, op: "conflict", reason: "both template and project changed",
					})
				case PolicyOurs:
					next.Files[p] = wantHash
					an.actions = append(an.actions, fileAction{
						path: p, op: "keep",
						reason: "both changed, policy=ours keeps project version",
					})
				case PolicyTheirs:
					next.Files[p] = wantHash
					an.actions = append(an.actions, fileAction{
						path: p, op: "update",
						reason: "both changed, policy=theirs applies template version",
						write:  true, content: want,
					})
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
			an.actions = append(an.actions, fileAction{
				path: p, op: "skip", reason: "removed from template, already absent",
			})
			continue
		}
		if readErr != nil {
			return nil, fmt.Errorf("read %s: %w", p, readErr)
		}
		if Hash(string(curBytes)) == manifest.Files[p] {
			delete(next.Files, p)
			an.actions = append(an.actions, fileAction{
				path: p, op: "delete", reason: "removed from template, project untouched",
				delete: true,
			})
		} else {
			delete(next.Files, p)
			an.actions = append(an.actions, fileAction{
				path: p, op: "keep", reason: "removed from template but modified by user",
			})
		}
	}

	sort.Strings(an.conflicts)
	return an, nil
}

// writeFile 先写临时文件再原子改名，避免写一半留下损坏内容。
// 开写前先清掉同名遗留临时文件，保证上次被中断（如进程被杀）后重跑仍然干净。
func writeFile(full, content string) error {
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("create dir for %s: %w", full, err)
	}
	tmp := full + ".tmp"
	_ = os.Remove(tmp)
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", full, err)
	}
	if err := os.Rename(tmp, full); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("commit %s: %w", full, err)
	}
	return nil
}
