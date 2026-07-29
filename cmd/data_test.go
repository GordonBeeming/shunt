package cmd

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

func TestConfirmDataChangeForceSkipsPrompt(t *testing.T) {
	var out bytes.Buffer
	if err := confirmDataChange(true, "replace baseline?", bufio.NewReader(strings.NewReader("")), &out); err != nil {
		t.Fatalf("confirmDataChange() error = %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("force output = %q, want no prompt", out.String())
	}
}

func TestDataCommandShape(t *testing.T) {
	command := newDataCmd()
	if command.Use != "data" || len(command.Commands()) != 2 {
		t.Fatalf("data command = %#v", command)
	}
}
