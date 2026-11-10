package main

import (
	"reflect"
	"testing"
)

func TestNormalizeArgsMovesFlagsAfterPositionals(t *testing.T) {
	got, err := normalizeArgs([]string{"D:\\Sync", "--data-dir", "D:\\State", "--interval=1m"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--data-dir", "D:\\State", "--interval=1m", "D:\\Sync"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestNormalizeArgsRejectsMissingFlagValue(t *testing.T) {
	if _, err := normalizeArgs([]string{"D:\\Sync", "--data-dir"}); err == nil {
		t.Fatal("expected missing flag value error")
	}
}
