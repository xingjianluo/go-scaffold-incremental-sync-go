package config

import (
	"errors"
	"reflect"
	"testing"
)

func mustLayer(t *testing.T, source, jsonText string) Layer {
	t.Helper()
	l, err := Decode(source, []byte(jsonText))
	if err != nil {
		t.Fatalf("Decode(%s): %v", source, err)
	}
	return l
}

func TestMergePriority(t *testing.T) {
	file := mustLayer(t, "file:scaffold.json",
		`{"projectName":"demo","modulePath":"example.com/demo","license":"Apache-2.0","features":["http","metrics"]}`)
	env := FromEnv("SCAFFOLD", func(k string) (string, bool) {
		if k == "SCAFFOLD_LICENSE" {
			return "BSD-3-Clause", true
		}
		return "", false
	})
	fs := NewFlagSet("test")
	if err := fs.Parse([]string{"-go", "1.26.0"}); err != nil {
		t.Fatal(err)
	}
	flags := FromFlags(fs)

	t.Logf("输入层(低→高): defaults=%s", String(Defaults().Params))
	t.Logf("输入层 file: %s set=%v", String(file.Params), file.Set)
	t.Logf("输入层 env: %s set=%v", String(env.Params), env.Set)
	t.Logf("输入层 flags: %s set=%v", String(flags.Params), flags.Set)

	got, err := Load(Defaults(), file, env, flags)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Logf("合并结果: %s", String(got))
	t.Logf("判定依据: 高层只覆盖显式设置的字段；license 来自 env，goVersion 来自 flags，其余来自 file")

	want := Params{
		ProjectName: "demo",
		ModulePath:  "example.com/demo",
		GoVersion:   "1.26.0",
		License:     "BSD-3-Clause",
		Features:    []string{"http", "metrics"},
		Labels:      map[string]string{},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("合并结果不符:\n got=%s\nwant=%s", String(got), String(want))
	}
}

func TestMergeDeterministic(t *testing.T) {
	mk := func() []Layer {
		return []Layer{
			Defaults(),
			mustLayer(t, "file:a.json",
				`{"projectName":"demo","modulePath":"example.com/demo","labels":{"team":"core","tier":"backend"}}`),
		}
	}
	first, err := Load(mk()...)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		got, err := Load(mk()...)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("第 %d 次合并结果不一致: %s vs %s", i, String(got), String(first))
		}
	}
	t.Logf("判定依据: 20 次重复合并输出完全一致: %s", String(first))
}

func TestRejectUnknownField(t *testing.T) {
	_, err := Decode("file:bad.json", []byte(`{"projectName":"demo","unknownKey":1}`))
	var ce *Error
	if !errors.As(err, &ce) {
		t.Fatalf("错误类型不符: %T %v", err, err)
	}
	t.Logf("输入: 含未知字段 unknownKey；拒绝原因: kind=%s msg=%s", ce.Kind, ce)
	if ce.Kind != KindUnknownField {
		t.Fatalf("kind=%s, want %s", ce.Kind, KindUnknownField)
	}
}

func TestRejectInvalidValues(t *testing.T) {
	cases := []struct {
		name  string
		json  string
		field string
	}{
		{"坏项目名", `{"projectName":"Demo_1","modulePath":"example.com/demo"}`, "projectName"},
		{"坏go版本", `{"projectName":"demo","modulePath":"example.com/demo","goVersion":"1.x"}`, "goVersion"},
		{"未知license", `{"projectName":"demo","modulePath":"example.com/demo","license":"GPLv9"}`, "license"},
		{"未知feature", `{"projectName":"demo","modulePath":"example.com/demo","features":["http","warp"]}`, "features"},
		{"重复feature", `{"projectName":"demo","modulePath":"example.com/demo","features":["http","http"]}`, "features"},
		{"坏标签键", `{"projectName":"demo","modulePath":"example.com/demo","labels":{"Bad Key":"v"}}`, "labels"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := mustLayer(t, "file:case.json", tc.json)
			_, err := Load(Defaults(), l)
			var ce *Error
			if !errors.As(err, &ce) {
				t.Fatalf("错误类型不符: %T %v", err, err)
			}
			t.Logf("输入: %s；拒绝原因: kind=%s field=%s", tc.json, ce.Kind, ce.Field)
			if ce.Kind != KindInvalidValue || ce.Field != tc.field {
				t.Fatalf("kind=%s field=%s, want %s/%s", ce.Kind, ce.Field, KindInvalidValue, tc.field)
			}
		})
	}
}

func TestRejectConflicts(t *testing.T) {
	cases := []struct {
		name string
		json string
	}{
		{"模块名与项目名矛盾", `{"projectName":"demo","modulePath":"example.com/other"}`},
		{"无license却声明版权", `{"projectName":"demo","modulePath":"example.com/demo","license":"none","labels":{"copyright":"acme"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := mustLayer(t, "file:case.json", tc.json)
			_, err := Load(Defaults(), l)
			var ce *Error
			if !errors.As(err, &ce) {
				t.Fatalf("错误类型不符: %T %v", err, err)
			}
			t.Logf("输入: %s；拒绝原因: kind=%s msg=%s", tc.json, ce.Kind, ce)
			if ce.Kind != KindConflict {
				t.Fatalf("kind=%s, want %s", ce.Kind, KindConflict)
			}
		})
	}
}

func TestLayerOnlyOverridesSetFields(t *testing.T) {
	base := mustLayer(t, "file:base.json",
		`{"projectName":"demo","modulePath":"example.com/demo","license":"Apache-2.0"}`)
	empty := Layer{Name: "flags", Set: map[string]bool{}} // 未设置任何字段
	got, err := Load(Defaults(), base, empty)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("判定依据: 空 flags 层不得覆盖 file 层；结果 license=%s", got.License)
	if got.License != "Apache-2.0" {
		t.Fatalf("license=%s, want Apache-2.0", got.License)
	}
}
