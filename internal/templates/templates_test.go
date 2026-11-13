package templates

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestNormalize(t *testing.T) {
	in, err := Normalize(Input{TemplateName: "TEAM/My_Template", Tag: new("V1.2"), Description: new(" \t\n\u00a0"), Active: true})
	if err != nil || in.TemplateName != "team/my_template" || *in.Tag != "v1.2" || in.Description != nil || !in.Active {
		t.Fatal(in, err)
	}
	for _, name := range []string{"", "a/b/c", " name", "name:tag", "шаблон", strings.Repeat("a", 256)} {
		if _, err := Normalize(Input{TemplateName: name}); err == nil {
			t.Fatalf("accepted name %q", name)
		}
	}
	for _, tag := range []string{"", "tag:1", "a/b", " tag", uuid.NewString(), strings.Repeat("t", 129)} {
		if _, err := Normalize(Input{TemplateName: "ok", Tag: &tag}); err == nil {
			t.Fatalf("accepted tag %q", tag)
		}
	}
	for _, description := range []string{"\x00", strings.Repeat("я", 4097), string([]byte{0xff})} {
		if _, err := Normalize(Input{TemplateName: "ok", Description: &description}); err == nil {
			t.Fatal("invalid description accepted")
		}
	}
	text := "  " + strings.Repeat("я", 4092) + "  "
	in, err = Normalize(Input{TemplateName: "ok", Description: &text})
	if err != nil || *in.Description != text || in.Tag != nil {
		t.Fatal(in, err)
	}
}

func TestPatch(t *testing.T) {
	for _, raw := range []string{`{}`, `null`, `[]`, `{"name":"no"}`, `{"tag":null}`, `{"active":null}`, `{"active":1}`, `{"description":false}`} {
		if _, err := ParsePatch([]byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	for _, raw := range []string{`{"description":null}`, `{"description":" \t "}`} {
		p, err := ParsePatch([]byte(raw))
		if err != nil || !p.SetDescription || p.Description != nil || p.Active != nil {
			t.Fatal(p, err)
		}
	}
	p, err := ParsePatch([]byte(`{"active":false}`))
	if err != nil || p.Active == nil || *p.Active || p.SetDescription {
		t.Fatal(p, err)
	}
}

func TestFilter(t *testing.T) {
	for _, f := range []Filter{{Name: new("")}, {NameContains: new(" \n")}, {Name: new("a\x00")}, {Name: new("a"), NameContains: new("a")}} {
		if f.Validate() == nil {
			t.Fatal(f)
		}
	}
	for _, term := range []string{"%", "_", `\`, "Юникод", "a"} {
		if err := (Filter{NameContains: &term}).Validate(); err != nil {
			t.Fatal(err)
		}
	}
}
