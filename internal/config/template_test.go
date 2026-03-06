package config

import (
	"strings"
	"testing"
)

func TestTemplateByNameLLMStack(t *testing.T) {
	tpl, err := TemplateByName("llm-stack")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tpl, "name: qdrant") {
		t.Fatalf("expected llm-stack template to include qdrant sidecar, got:\n%s", tpl)
	}
}

func TestTemplateByNameUnknown(t *testing.T) {
	_, err := TemplateByName("does-not-exist")
	if err == nil {
		t.Fatal("expected unknown template error")
	}
	if !strings.Contains(err.Error(), "llm-stack") {
		t.Fatalf("expected supported template list in error, got %q", err.Error())
	}
}
