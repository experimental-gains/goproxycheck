package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestModuleFromGoMod(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "go.mod")
	os.WriteFile(path, []byte("module github.com/experimental-gains/goproxycheck\n\ngo 1.24\n"), 0o644)

	got, err := moduleFromGoMod(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "github.com/experimental-gains/goproxycheck"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestModuleFromGoMod_Missing(t *testing.T) {
	if _, err := moduleFromGoMod(filepath.Join(t.TempDir(), "go.mod")); err == nil {
		t.Fatal("expected an error for a missing go.mod")
	}
}

func TestResolveTarget_ExplicitArg(t *testing.T) {
	module, version, err := resolveTarget([]string{"example.com/mod@v1.2.3"})
	if err != nil {
		t.Fatal(err)
	}
	if module != "example.com/mod" || version != "v1.2.3" {
		t.Errorf("got (%q, %q)", module, version)
	}
}

func TestResolveTarget_BadArg(t *testing.T) {
	if _, _, err := resolveTarget([]string{"no-at-sign"}); err == nil {
		t.Fatal("expected an error for an argument without '@'")
	}
}

func TestResolveTarget_TooManyArgs(t *testing.T) {
	if _, _, err := resolveTarget([]string{"a@1", "b@2"}); err == nil {
		t.Fatal("expected an error for more than one argument")
	}
}

func TestResolveTarget_FallbackToGoModAndGitTag(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	os.WriteFile("go.mod", []byte("module example.com/fallback\n\ngo 1.24\n"), 0o644)
	run := func(name string, args ...string) {
		t.Helper()
		if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
	}
	run("git", "init", "-q")
	run("git", "config", "user.email", "test@example.com")
	run("git", "config", "user.name", "test")
	run("git", "add", "go.mod")
	run("git", "commit", "-q", "-m", "init")
	run("git", "tag", "v0.9.0")

	module, version, err := resolveTarget(nil)
	if err != nil {
		t.Fatal(err)
	}
	if module != "example.com/fallback" || version != "v0.9.0" {
		t.Errorf("got (%q, %q)", module, version)
	}
}

func TestResolveTarget_NoGoMod(t *testing.T) {
	t.Chdir(t.TempDir())
	if _, _, err := resolveTarget(nil); err == nil {
		t.Fatal("expected an error with no go.mod present")
	}
}
