package main

import (
	"encoding/json"
	"os"

	"github.com/jnesspace/admin-stack-splitter/internal/splitter"
)

func main() {
	if err := splitter.Run(os.Args[1:], os.Stdout); err != nil {
		_ = json.NewEncoder(os.Stderr).Encode(map[string]string{"error": err.Error()})
		os.Exit(1)
	}
}
