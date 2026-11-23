package main

import (
	"os"
	"testing"
	"time"
)

func TestASecondCopyBringsTheFirstToTheFront(t *testing.T) {
	dir, err := os.MkdirTemp("", "tc") // short: a socket path has a length limit
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	fronted := make(chan bool, 1)
	if !oneCopy(dir, func() { fronted <- true }) {
		t.Fatal("the first copy must open")
	}
	if oneCopy(dir, func() {}) {
		t.Fatal("the second copy must not open")
	}
	select {
	case <-fronted:
	case <-time.After(3 * time.Second):
		t.Fatal("the first copy was not asked to come to the front")
	}
}
