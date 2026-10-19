package testproto

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func vector(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "vectors", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestGoldenVectors(t *testing.T) {
	a, s := SampleAgent(), SampleStatus()
	ab, _ := a.MarshalBinary()
	sb, _ := s.MarshalBinary()
	if !bytes.Equal(ab, vector(t, "testproto_agent.bin")) {
		t.Error("agent encoding differs from the golden vector")
	}
	if !bytes.Equal(sb, vector(t, "testproto_status.bin")) {
		t.Error("status encoding differs from the golden vector")
	}
	var a2 Agent
	var s2 Status
	if err := a2.UnmarshalBinary(vector(t, "testproto_agent.bin")); err != nil || a2 != a {
		t.Errorf("agent decode: %+v %v", a2, err)
	}
	if err := s2.UnmarshalBinary(vector(t, "testproto_status.bin")); err != nil || s2 != s {
		t.Errorf("status decode: %+v %v", s2, err)
	}
	if s.Getpid >= 0 || s.Deltas[0] >= 0 {
		t.Error("sample should exercise sign bits")
	}
}

// The checked-in C header, Go code and vectors must match the schema.
func TestGeneratedUpToDate(t *testing.T) {
	root := filepath.Join("..", "..")
	dir := t.TempDir()
	cmd := exec.Command("go", "run", "./cmd/procctl-gen", "-schema", "schemas/testproto.procctl.json",
		"-c-out", dir, "-go-out", dir, "-go-package", "testproto", "-vectors", dir)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for gen, committed := range map[string]string{
		"testproto_schema.h":   "c/gen/testproto_schema.h",
		"schema_gen.go":        "internal/testproto/schema_gen.go",
		"testproto_agent.bin":  "testdata/vectors/testproto_agent.bin",
		"testproto_status.bin": "testdata/vectors/testproto_status.bin",
	} {
		a, _ := os.ReadFile(filepath.Join(dir, gen))
		b, _ := os.ReadFile(filepath.Join(root, committed))
		if !bytes.Equal(a, b) {
			t.Errorf("%s is stale: re-run procctl-gen", committed)
		}
	}
}
