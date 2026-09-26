package config

import (
	"os"
	"errors"
	"reflect"
	"testing"
)

// layer 是测试用的内存来源。
func layer(name string, set map[string]any) Source {
	return &mapSource{name: name, set: set}
}

func TestLoadMergePriority(t *testing.T) {
	cases := []struct {
		name    string
		sources []Source
		want    *Params
	}{
		{
			name:    "defaults only",
			sources: []Source{Defaults()},
			want: &Params{
				ProjectName: "app", Module: "example.com/app", GoVersion: "1.22",
				License: "MIT", WithGit: true, Features: []string{},
			},
		},
		{
			name: "file overrides defaults, flags override file",
			sources: []Source{
				Defaults(),
				layer("file", map[string]any{"project_name": "from-file", "module": "example.com/file"}),
				layer("flags", map[string]any{"project_name": "from-flags"}),
			},
			want: &Params{
				ProjectName: "from-flags", Module: "example.com/file", GoVersion: "1.22",
				License: "MIT", WithGit: true, Features: []string{},
			},
		},
		{
			name: "features sorted for determinism",
			sources: []Source{
				Defaults(),
				layer("flags", map[string]any{"features": []any{"zeta", "alpha", "mid"}}),
			},
			want: &Params{
				ProjectName: "app", Module: "example.com/app", GoVersion: "1.22",
				License: "MIT", WithGit: true, Features: []string{"alpha", "mid", "zeta"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Load(tc.sources...)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			t.Logf("merged result: %+v", got)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("merged mismatch\n got: %+v\nwant: %+v", got, tc.want)
			}
		})
	}
}

func TestLoadDeterministic(t *testing.T) {
	sources := func() []Source {
		return []Source{
			Defaults(),
			layer("file", map[string]any{"features": []any{"b", "a"}, "module": "example.com/x"}),
			layer("flags", map[string]any{"project_name": "det"}),
		}
	}
	first, err := Load(sources()...)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for i := 0; i < 20; i++ {
		got, err := Load(sources()...)
		if err != nil {
			t.Fatalf("Load run %d: %v", i, err)
		}
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d differs: %+v vs %+v", i, got, first)
		}
	}
	t.Logf("20 repeated loads identical: %+v", first)
}

func TestLoadRejections(t *testing.T) {
	cases := []struct {
		name     string
		set      map[string]any
		wantKind Kind
	}{
		{"unknown field", map[string]any{"nonsense": 1}, KindUnknownField},
		{"wrong type", map[string]any{"project_name": true}, KindInvalidValue},
		{"empty name", map[string]any{"project_name": ""}, KindInvalidValue},
		{"bad module chars", map[string]any{"module": "bad module!"}, KindInvalidValue},
		{"bad go version", map[string]any{"go_version": "latest"}, KindInvalidValue},
		{"bad license", map[string]any{"license": "WTFPL"}, KindInvalidValue},
		{"features not strings", map[string]any{"features": []any{"ok", 3}}, KindInvalidValue},
		{"contradiction git feature without git", map[string]any{
			"with_git": false, "features": []any{"git"}}, KindConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Logf("input combination: %v", tc.set)
			_, err := Load(Defaults(), layer("test", tc.set))
			if err == nil {
				t.Fatalf("expected rejection, got success")
			}
			var ce *Error
			if !errors.As(err, &ce) {
				t.Fatalf("error type = %T, want *config.Error", err)
			}
			t.Logf("rejected as kind=%s field=%q reason=%q", ce.Kind, ce.Field, ce.Reason)
			if ce.Kind != tc.wantKind {
				t.Fatalf("kind = %s, want %s", ce.Kind, tc.wantKind)
			}
		})
	}
}

func TestFileSource(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.json"
	write := func(s string) { 
		if err := os.WriteFile(path, []byte(s), 0o644); err != nil { t.Fatal(err) }
	}

	write(`{"project_name": "fromjson", "module": "example.com/json"}`)
	p, err := Load(Defaults(), &FileSource{Path: path})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Logf("file layer merged: %+v", p)
	if p.ProjectName != "fromjson" || p.Module != "example.com/json" {
		t.Fatalf("unexpected params: %+v", p)
	}

	write(`{"project_name": 42}`)
	_, err = Load(Defaults(), &FileSource{Path: path})
	var ce *Error
	if !errors.As(err, &ce) || ce.Kind != KindInvalidValue {
		t.Fatalf("numeric field should be invalid_value, got %v", err)
	}
	t.Logf("numeric field rejected: %v", ce)

	write(`{"unknown_key": true}`)
	_, err = Load(Defaults(), &FileSource{Path: path})
	if !errors.As(err, &ce) || ce.Kind != KindUnknownField {
		t.Fatalf("unknown key should be unknown_field, got %v", err)
	}
	t.Logf("unknown key rejected: %v", ce)

	write(`{broken`)
	_, err = Load(Defaults(), &FileSource{Path: path})
	if !errors.As(err, &ce) || ce.Kind != KindSourceFailure {
		t.Fatalf("broken json should be source_failure, got %v", err)
	}
	t.Logf("broken json rejected: %v", ce)
}
