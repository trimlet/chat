package main

import (
	"path/filepath"
	"testing"
)

func TestTheKeyIsSavedAndReadBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trimletchat", "access.json")
	a, err := loadAccess(path)
	if err != nil || a.Key != "" {
		t.Fatal(err)
	}
	a.Key = aKey
	if err := a.save(); err != nil {
		t.Fatal(err)
	}
	b, err := loadAccess(path)
	if err != nil || b.Key != aKey {
		t.Fatalf("%v %q", err, b.Key)
	}
}
