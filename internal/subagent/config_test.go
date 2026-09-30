package subagent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSpecValidate(t *testing.T) {
	if err := (Spec{Name: "probe"}).Validate(); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
	for _, bad := range []Spec{
		{Name: "  "},
		{Name: "has space"},
		{Name: `quo"te`},
		{Name: "probe", Effort: "turbo"},
	} {
		if err := bad.Validate(); err == nil {
			t.Fatalf("invalid spec accepted: %+v", bad)
		}
	}
	// Effort must be one of the loop's levels (empty = default).
	for _, ok := range []string{"", "off", "low", "medium", "high"} {
		s := Spec{Name: "probe", Effort: ok}
		if err := s.Validate(); err != nil {
			t.Fatalf("effort %q rejected: %v", ok, err)
		}
	}
}

func TestAllMergesBuiltins(t *testing.T) {
	// Empty file: the built-in is the whole surface.
	merged := All(nil)
	if len(merged) != 1 || merged[0].Name != "general-purpose" || !IsBuiltin("general-purpose") {
		t.Fatalf("built-in surface = %+v", merged)
	}
	if IsBuiltin("researcher") {
		t.Fatal("IsBuiltin leaks to user names")
	}

	// A same-name entry OVERRIDES the built-in; extra names append;
	// the result stays sorted.
	merged = All([]Spec{
		{Name: "researcher", Effort: "high"},
		{Name: "general-purpose", Model: "openai-compat:m2"},
	})
	if len(merged) != 2 {
		t.Fatalf("merged = %+v", merged)
	}
	if merged[0].Name != "general-purpose" || merged[0].Model != "openai-compat:m2" {
		t.Fatalf("override did not win: %+v", merged[0])
	}
	if merged[1].Name != "researcher" || merged[1].Effort != "high" {
		t.Fatalf("user agent lost: %+v", merged[1])
	}
}

func TestAgentsFileRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.json")

	specs, err := Load(path)
	if err != nil || len(specs) != 0 {
		t.Fatalf("absent file: specs=%v err=%v", specs, err)
	}

	a := Spec{Name: "researcher", Description: "代码考古", Model: "openai-compat:m2", Effort: "high"}
	if err := Upsert(path, a); err != nil {
		t.Fatal(err)
	}
	if err := Upsert(path, Spec{Name: "prober", Model: ""}); err != nil {
		t.Fatal(err)
	}
	specs, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2 || specs[0].Name != "prober" || specs[1].Name != "researcher" {
		t.Fatalf("roundtrip = %+v", specs)
	}
	if specs[1].Model != "openai-compat:m2" || specs[1].Effort != "high" || specs[1].Description != "代码考古" {
		t.Fatalf("fields lost: %+v", specs[1])
	}

	// Upsert REPLACES by name, unknown document keys survive.
	if err := os.WriteFile(path, []byte(`{"agents":[{"name":"prober","model":"x:y"},{"name":"researcher","model":"openai-compat:m2","effort":"high","description":"代码考古"}],"futureKnob":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Upsert(path, Spec{Name: "prober", Model: "a:b"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	body := string(data)
	if !strings.Contains(body, `"futureKnob": true`) || strings.Contains(body, `"x:y"`) {
		t.Fatalf("RMW broke the document:\n%s", body)
	}

	if err := Delete(path, "prober"); err != nil {
		t.Fatal(err)
	}
	specs, _ = Load(path)
	if len(specs) != 1 || specs[0].Name != "researcher" {
		t.Fatalf("delete left = %+v", specs)
	}
	if err := Delete(path, "missing"); err != nil {
		t.Fatalf("deleting an absent name errored: %v", err)
	}
}
