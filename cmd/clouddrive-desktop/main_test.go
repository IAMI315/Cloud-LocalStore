package main

import (
	"bytes"
	"testing"

	"github.com/IAMI315/Cloud-LocalStore/internal/desktop"
)

func TestRunDefaultsToDesktopGUI(t *testing.T) {
	var out bytes.Buffer
	options, err := parseOptions(nil, &out)
	if err != nil {
		t.Fatal(err)
	}
	if options != (desktop.Options{Addr: desktop.DefaultAddr}) {
		t.Fatalf("default options = %#v", options)
	}
}

func TestParseOptionsRejectsPositionalArguments(t *testing.T) {
	var out bytes.Buffer
	if _, err := parseOptions([]string{"unexpected"}, &out); err == nil {
		t.Fatal("expected usage error")
	}
}
