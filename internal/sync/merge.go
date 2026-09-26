package sync

import (
	"strings"
)

// MergeResult 是三方合并的结果。
type MergeResult struct {
	Lines     []string
	Conflicts int // 冲突块数量
}

// Merge3 做行级三方合并：base 为共同祖先，ours 为本地版本，theirs 为模板版本。
//
// 算法：分别对 base→ours、base→theirs 做 LCS 行级 diff，然后把每个
// base 行连同其前置插入视为一个“区域”，两侧对同一区域的修改做三方裁决：
//   - 两侧相同：直接采用
//   - 仅一侧偏离 base：采用该侧
//   - 两侧不一致地偏离 base：输出冲突标记块
func Merge3(base, ours, theirs string) MergeResult {
	b := splitLines(base)
	delO, insO := edits(b, splitLines(ours))
	delT, insT := edits(b, splitLines(theirs))

	var out []string
	conflicts := 0
	for bi := 0; bi <= len(b); bi++ {
		baseRegion := sliceAt(b, bi)
		oursRegion := region(insO[bi], delO[bi], baseRegion)
		theirsRegion := region(insT[bi], delT[bi], baseRegion)
		switch {
		case equal(oursRegion, theirsRegion):
			out = append(out, oursRegion...)
		case equal(oursRegion, baseRegion):
			out = append(out, theirsRegion...)
		case equal(theirsRegion, baseRegion):
			out = append(out, oursRegion...)
		default:
			conflicts++
			out = append(out, "<<<<<<< local")
			out = append(out, oursRegion...)
			out = append(out, "=======")
			out = append(out, theirsRegion...)
			out = append(out, ">>>>>>> template")
		}
	}
	return MergeResult{Lines: out, Conflicts: conflicts}
}

// String 将合并结果拼回文本。
func (m MergeResult) String() string {
	if len(m.Lines) == 0 {
		return ""
	}
	return strings.Join(m.Lines, "\n") + "\n"
}

// region 计算一侧对某个 base 区域的替换结果：
// 前置插入行 + （未删除时的）base 行。
func region(ins []string, deleted bool, baseRegion []string) []string {
	out := append([]string(nil), ins...)
	if !deleted {
		out = append(out, baseRegion...)
	}
	return out
}

// edits 计算 base→next 的行级变更，返回：
// del[i]=true 表示 base 第 i 行被删除；ins[i] 为在 base 第 i 行之前插入的行。
func edits(base, next []string) (del map[int]bool, ins map[int][]string) {
	del = map[int]bool{}
	ins = map[int][]string{}
	n, m := len(base), len(next)
	// LCS 长度表
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			switch {
			case base[i] == next[j]:
				dp[i][j] = dp[i+1][j+1] + 1
			case dp[i][j+1] >= dp[i+1][j]:
				dp[i][j] = dp[i][j+1]
			default:
				dp[i][j] = dp[i+1][j]
			}
		}
	}
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case base[i] == next[j]:
			i++
			j++
		case dp[i][j+1] >= dp[i+1][j]:
			// 平局时优先插入：让“修改”锚定在被改的 base 行上
			ins[i] = append(ins[i], next[j])
			j++
		default:
			del[i] = true
			i++
		}
	}
	for ; i < n; i++ {
		del[i] = true
	}
	for ; j < m; j++ {
		ins[n] = append(ins[n], next[j])
	}
	return del, ins
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

func sliceAt(b []string, i int) []string {
	if i < len(b) {
		return []string{b[i]}
	}
	return nil
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
