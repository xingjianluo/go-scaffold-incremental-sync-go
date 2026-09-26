package sync

import (
	"strings"
	"testing"
)

func TestMerge3(t *testing.T) {
	cases := []struct {
		name           string
		base, ours, th string
		want           string
		conflicts      int
	}{
		{
			name: "仅模板侧改",
			base: "a\nb\nc\n", ours: "a\nb\nc\n", th: "a\nB\nc\n",
			want: "a\nB\nc\n", conflicts: 0,
		},
		{
			name: "仅本地改",
			base: "a\nb\nc\n", ours: "a\nb2\nc\n", th: "a\nb\nc\n",
			want: "a\nb2\nc\n", conflicts: 0,
		},
		{
			name: "两侧改不同行",
			base: "a\nb\nc\n", ours: "A\nb\nc\n", th: "a\nb\nC\n",
			want: "A\nb\nC\n", conflicts: 0,
		},
		{
			name: "两侧改同一行",
			base: "a\nb\nc\n", ours: "a\nours\nc\n", th: "a\ntheirs\nc\n",
			want:      "a\n<<<<<<< local\nours\n=======\ntheirs\n>>>>>>> template\nc\n",
			conflicts: 1,
		},
		{
			name: "两侧相同修改",
			base: "a\nb\n", ours: "a\nx\n", th: "a\nx\n",
			want: "a\nx\n", conflicts: 0,
		},
		{
			name: "本地删除模板未动",
			base: "a\nb\nc\n", ours: "a\nc\n", th: "a\nb\nc\n",
			want: "a\nc\n", conflicts: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Merge3(tc.base, tc.ours, tc.th)
			t.Logf("输入 base=%q ours=%q theirs=%q", tc.base, tc.ours, tc.th)
			t.Logf("合并结果=%q 冲突数=%d", got.String(), got.Conflicts)
			if got.String() != tc.want || got.Conflicts != tc.conflicts {
				t.Fatalf("got %q conflicts=%d, want %q conflicts=%d",
					got.String(), got.Conflicts, tc.want, tc.conflicts)
			}
			if tc.conflicts > 0 && !strings.Contains(got.String(), "<<<<<<< local") {
				t.Fatal("冲突块缺少标记")
			}
		})
	}
}
