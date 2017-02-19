package bridge

import (
	"github.com/HouzuoGuo/websh/feature"
	"testing"
)

func CommandPINOrShortcut_Transform(t *testing.T) {
	pin := CommandPINOrShortcut{PIN: "mypin"}
	if out, err := pin.Transform(feature.Command{Content: "abc"}); err != ErrPINAndShortcutNotFound || out.Content != "" {
		t.Fatal(out)
	}
	if out, err := pin.Transform(feature.Command{Content: "mypineapple"}); err != nil || out.Content != "eapple" {
		t.Fatal(out)
	}
	if out, err := pin.Transform(feature.Command{Content: "\n\n mypineapple \n\n"}); err != nil || out.Content != "eapple" {
		t.Fatal(out)
	}
	pin.Shortcuts = map[string]string{"abc": "123", "def": "456"}
	if out, err := pin.Transform(feature.Command{Content: "nothing_to_see"}); err != ErrPINAndShortcutNotFound || out.Content != "" {
		t.Fatal(out)
	}
	if out, err := pin.Transform(feature.Command{Content: "\n\n mypineapple \n\n"}); err != nil || out.Content != "eapple" {
		t.Fatal(out)
	}
	if out, err := pin.Transform(feature.Command{Content: "\n\n abc"}); err != nil || out.Content != "123" {
		t.Fatal(out)
	}
	if out, err := pin.Transform(feature.Command{Content: "\n\n def \n\n"}); err != nil || out.Content != "456" {
		t.Fatal(out)
	}
	if out, err := pin.Transform(feature.Command{Content: "ghi"}); err != nil || out.Content != "ghi" {
		t.Fatal(out)
	}
}

func TestCommandTranslator_Transform(t *testing.T) {
	tr := CommandTranslator{}
	if out, err := tr.Transform(feature.Command{Content: "abc"}); err != nil || out.Content != "abc" {
		t.Fatal(out)
	}
	tr.Sequences = [][]string{{"abc", "123"}, {"def", "456"}}
	if out, err := tr.Transform(feature.Command{Content: ""}); err != nil || out.Content != "" {
		t.Fatal(out)
	}
	if out, err := tr.Transform(feature.Command{Content: " abc def "}); err != nil || out.Content != " 123 456 " {
		t.Fatal(out)
	}
	if out, err := tr.Transform(feature.Command{Content: " ghi "}); err != nil || out.Content != " ghi " {
		t.Fatal(out)
	}
}