package main

import (
	"os"

	"github.com/jnesspace/admin-stack-splitter/internal/splitter"
)

func main() {
	os.Exit(splitter.Execute(os.Args[1:], os.Stdout, os.Stderr))
}
