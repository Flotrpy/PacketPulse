package main

import (
	"os"
	"slices"
	"testing"
)

func TestWithoutSelf(t *testing.T) {
	self, _ := os.Executable()
	kept, dropped := withoutSelf([]string{self, "-server", "https://x", os.Args[0], "example.com"})
	if !slices.Equal(kept, []string{"-server", "https://x", "example.com"}) || len(dropped) != 2 {
		t.Fatalf("kept %v dropped %v", kept, dropped)
	}
}

func TestLooksLikeTarget(t *testing.T) {
	for s, ok := range map[string]bool{
		"google.com": true, "1.1.1.1": true, "2606:4700::1111": true, "bbc.co.uk.": true,
		`C:\Users\affan\Downloads\ppagent.exe`: false, "./ppagent": false, "https://x.com": false, "": false, "a..b": false,
	} {
		if looksLikeTarget(s) != ok {
			t.Errorf("%q: want %v", s, ok)
		}
	}
}
