package main

import (
	_ "embed"
	"os"
	"strings"
)

//go:embed spire.txt
var spireLogo string

func currentDirName() string {
	dir, err := os.Getwd()
	if err != nil {
		return "hub"
	}
	parts := strings.Split(dir, string(os.PathSeparator))
	if len(parts) > 0 {
		return parts[len(parts)-1]
	}
	return "hub"
}
